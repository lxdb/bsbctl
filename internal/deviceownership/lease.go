// Package deviceownership prevents multiple bsbctl daemons from writing to
// the same BUSY Bar application namespace at the same time.
package deviceownership

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const maxOwnerRecordBytes = 32

// ErrAlreadyOwned reports that another bsbctl daemon currently owns the
// device/application pair.
var ErrAlreadyOwned = errors.New("device application is already owned")

// ConflictError carries the safe portion of the current owner's identity.
// Device addresses and credentials never appear in this error.
type ConflictError struct {
	OwnerPID int
}

func (err *ConflictError) Error() string {
	if err.OwnerPID > 0 {
		return fmt.Sprintf("%v by bsbctl process %d", ErrAlreadyOwned, err.OwnerPID)
	}
	return ErrAlreadyOwned.Error()
}

func (err *ConflictError) Unwrap() error { return ErrAlreadyOwned }

// Lease holds exclusive ownership until Close or process exit.
type Lease struct {
	directoryFD int
	lockFD      int
	closeOnce   sync.Once
	closeErr    error
}

// Acquire obtains the per-user, process-lifetime device/application lease.
// The location is intentionally independent of daemon config and socket paths.
func Acquire(baseURL, application string) (*Lease, error) {
	uid := os.Getuid()
	base := filepath.Join("/tmp", "bsbctl-"+strconv.Itoa(uid))
	baseFD, err := openPrivateDirectory(base, uid)
	if err != nil {
		return nil, err
	}
	if err := unix.Close(baseFD); err != nil {
		return nil, errors.New("close device ownership base directory")
	}
	root := filepath.Join(base, "device-ownership")
	return acquire(root, uid, baseURL, application)
}

func acquire(root string, uid int, baseURL, application string) (*Lease, error) {
	identity, err := canonicalIdentity(baseURL, application)
	if err != nil {
		return nil, err
	}
	directoryFD, err := openPrivateDirectory(root, uid)
	if err != nil {
		return nil, err
	}
	closeDirectory := true
	defer func() {
		if closeDirectory {
			_ = unix.Close(directoryFD)
		}
	}()

	name := "device-" + identity + ".lock"
	lockFD, err := unix.Openat(directoryFD, name, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		lockFD, err = unix.Openat(directoryFD, name, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, errors.New("open device ownership record")
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = unix.Close(lockFD)
		}
	}()
	if created {
		if err := unix.Fchmod(lockFD, 0o600); err != nil {
			return nil, errors.New("protect device ownership record")
		}
	}
	before, err := inspectLock(lockFD, uid)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &ConflictError{OwnerPID: readOwnerPID(lockFD)}
		}
		return nil, errors.New("lock device ownership record")
	}
	locked := true
	defer func() {
		if locked {
			_ = unix.Flock(lockFD, unix.LOCK_UN)
		}
	}()
	after, err := inspectLock(lockFD, uid)
	if err != nil || before.Dev != after.Dev || before.Ino != after.Ino {
		return nil, errors.New("device ownership record changed during acquisition")
	}
	if err := writeOwnerPID(lockFD, os.Getpid()); err != nil {
		return nil, err
	}

	locked = false
	closeLock = false
	closeDirectory = false
	return &Lease{directoryFD: directoryFD, lockFD: lockFD}, nil
}

func openPrivateDirectory(path string, uid int) (int, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return -1, errors.New("create device ownership directory")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, errors.New("open device ownership directory")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR || uint32(stat.Mode)&0o7777 != 0o700 || int(stat.Uid) != uid {
		_ = unix.Close(fd)
		return -1, errors.New("device ownership directory is unsafe")
	}
	return fd, nil
}

func inspectLock(fd, uid int) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG || uint32(stat.Mode)&0o7777 != 0o600 || int(stat.Uid) != uid || stat.Size < 0 || stat.Size > maxOwnerRecordBytes {
		return unix.Stat_t{}, errors.New("device ownership record is unsafe")
	}
	return stat, nil
}

func writeOwnerPID(fd, pid int) error {
	if err := unix.Ftruncate(fd, 0); err != nil {
		return errors.New("reset device ownership record")
	}
	value := []byte(strconv.Itoa(pid) + "\n")
	if count, err := unix.Pwrite(fd, value, 0); err != nil || count != len(value) {
		return errors.New("write device ownership record")
	}
	if err := unix.Fsync(fd); err != nil {
		return errors.New("sync device ownership record")
	}
	return nil
}

func readOwnerPID(fd int) int {
	buffer := make([]byte, maxOwnerRecordBytes)
	count, err := unix.Pread(fd, buffer, 0)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buffer[:count])))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func canonicalIdentity(baseURL, application string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("device URL is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("device URL is invalid")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if address := net.ParseIP(host); address != nil {
		host = address.String()
	}
	if host == "" || strings.TrimSpace(application) == "" {
		return "", errors.New("device ownership identity is invalid")
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	digest := sha256.Sum256([]byte(host + "\x00" + port + "\x00" + application))
	return fmt.Sprintf("%x", digest), nil
}

// Close releases the lease. It is safe to call more than once.
func (lease *Lease) Close() error {
	if lease == nil {
		return nil
	}
	lease.closeOnce.Do(func() {
		lease.closeErr = errors.Join(
			unix.Flock(lease.lockFD, unix.LOCK_UN),
			unix.Close(lease.lockFD),
			unix.Close(lease.directoryFD),
		)
	})
	return lease.closeErr
}
