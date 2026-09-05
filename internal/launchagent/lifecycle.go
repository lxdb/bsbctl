package launchagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	maxLifecycleLockBytes       = 1024
	initialLifecycleLockBackoff = 2 * time.Millisecond
	maxLifecycleLockBackoff     = 25 * time.Millisecond
)

type lifecycleLockOptions struct {
	beforeLock         func()
	afterLock          func()
	waitForRetry       func(context.Context, time.Duration) error
	observeDescriptors func(int, int)
}

type plistLifecycle struct {
	directoryFD       int
	directoryPath     string
	directoryIdentity fileIdentity
	name              string
	lockFD            int
	uid               int
}

// The lifecycle lock serializes cooperating bsbctl processes and accidental
// races. It does not claim protection from a hostile process running as the
// same user, which already controls the user's plist, binary, and config.

func lifecycleLockName(plistName string) string {
	digest := sha256.Sum256([]byte(plistName))
	return fmt.Sprintf(".bsbctl-launchagent-lock-%x", digest)
}

func acquirePlistLifecycle(ctx context.Context, path string, uid int, options lifecycleLockOptions) (*plistLifecycle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directoryPath := filepath.Dir(path)
	if err := os.MkdirAll(directoryPath, 0o700); err != nil {
		return nil, errors.New("create LaunchAgent directory")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directoryFD, err := unix.Open(directoryPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("open LaunchAgent directory")
	}
	closeDirectory := true
	defer func() {
		if closeDirectory {
			_ = unix.Close(directoryFD)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var directoryStat unix.Stat_t
	if err := unix.Fstat(directoryFD, &directoryStat); err != nil || uint32(directoryStat.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.New("inspect LaunchAgent directory")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockName := lifecycleLockName(filepath.Base(path))
	lockFD, err := unix.Openat(directoryFD, lockName, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		lockFD, err = unix.Openat(directoryFD, lockName, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, errors.New("open LaunchAgent lifecycle lock")
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = unix.Close(lockFD)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if created {
		if err := unix.Fchmod(lockFD, 0o600); err != nil {
			return nil, errors.New("protect LaunchAgent lifecycle lock")
		}
	}
	var before unix.Stat_t
	if err := unix.Fstat(lockFD, &before); err != nil || uint32(before.Mode)&unix.S_IFMT != unix.S_IFREG || uint32(before.Mode)&0o7777 != 0o600 || int(before.Uid) != uid || before.Size < 0 || before.Size > maxLifecycleLockBytes {
		return nil, errors.New("invalid LaunchAgent lifecycle lock")
	}
	if options.observeDescriptors != nil {
		options.observeDescriptors(directoryFD, lockFD)
	}
	if options.beforeLock != nil {
		options.beforeLock()
	}
	waitForRetry := options.waitForRetry
	if waitForRetry == nil {
		waitForRetry = waitForLifecycleLockRetry
	}
	backoff := initialLifecycleLockBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("lock LaunchAgent lifecycle")
		}
		if err := waitForRetry(ctx, backoff); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, errors.New("wait for LaunchAgent lifecycle lock")
		}
		if backoff < maxLifecycleLockBackoff {
			backoff *= 2
			if backoff > maxLifecycleLockBackoff {
				backoff = maxLifecycleLockBackoff
			}
		}
	}
	if err := ctx.Err(); err != nil {
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		return nil, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(lockFD, &after); err != nil || identityFromStat(before) != identityFromStat(after) || before.Mode != after.Mode || before.Uid != after.Uid || after.Size < 0 || after.Size > maxLifecycleLockBytes {
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		return nil, errors.New("invalid LaunchAgent lifecycle lock")
	}
	if options.afterLock != nil {
		options.afterLock()
	}
	if err := ctx.Err(); err != nil {
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		return nil, err
	}
	closeDirectory = false
	closeLock = false
	return &plistLifecycle{
		directoryFD:       directoryFD,
		directoryPath:     directoryPath,
		directoryIdentity: identityFromStat(directoryStat),
		name:              filepath.Base(path),
		lockFD:            lockFD,
		uid:               uid,
	}, nil
}

func waitForLifecycleLockRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (lifecycle *plistLifecycle) pathStillPinned() bool {
	directoryFD, err := unix.Open(lifecycle.directoryPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	var stat unix.Stat_t
	statErr := unix.Fstat(directoryFD, &stat)
	closeErr := unix.Close(directoryFD)
	return statErr == nil && closeErr == nil && identityFromStat(stat) == lifecycle.directoryIdentity
}

func (lifecycle *plistLifecycle) close() error {
	if lifecycle == nil {
		return nil
	}
	unlockErr := unix.Flock(lifecycle.lockFD, unix.LOCK_UN)
	lockCloseErr := unix.Close(lifecycle.lockFD)
	directoryCloseErr := unix.Close(lifecycle.directoryFD)
	return errors.Join(unlockErr, lockCloseErr, directoryCloseErr)
}
