package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

const (
	machHeader64Size = 32
	machMagic64      = 0xfeedfacf
	machLCUUID       = 0x1b
	machUUIDCmdSize  = 24
)

var codesignExecutable = "/usr/bin/codesign"

var errCodesignUnavailable = errors.New("required Apple codesign tool is unavailable")

func finalizeDarwinReleaseComponent(ctx context.Context, path, goos string) error {
	if goos != "darwin" {
		return nil
	}
	if err := validateCodesignTool(); err != nil {
		return err
	}
	if err := addDeterministicMachOUUID(path); err != nil {
		return err
	}
	return adHocSignDarwinComponent(ctx, path)
}

func validateCodesignTool() error {
	info, err := os.Stat(codesignExecutable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errCodesignUnavailable
	}
	return nil
}

func adHocSignDarwinComponent(ctx context.Context, path string) error {
	command := exec.CommandContext(ctx, codesignExecutable, "--force", "--sign", "-", path)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	output = bytes.TrimSpace(output)
	if len(output) > 4096 {
		output = output[:4096]
	}
	if len(output) == 0 {
		return errors.New("ad-hoc sign deterministic Darwin release binary")
	}
	return fmt.Errorf("ad-hoc sign deterministic Darwin release binary: %s", output)
}

func addDeterministicMachOUUID(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) < machHeader64Size || binary.LittleEndian.Uint32(data[:4]) != machMagic64 {
		return errors.New("release binary is not a little-endian 64-bit Mach-O")
	}
	ncmds := binary.LittleEndian.Uint32(data[16:20])
	sizeofcmds := binary.LittleEndian.Uint32(data[20:24])
	loadEnd := machHeader64Size + int(sizeofcmds)
	if loadEnd < machHeader64Size || loadEnd+machUUIDCmdSize > len(data) {
		return errors.New("release Mach-O load commands are malformed")
	}
	offset := machHeader64Size
	for range ncmds {
		if offset+8 > loadEnd {
			return errors.New("release Mach-O load command is truncated")
		}
		command := binary.LittleEndian.Uint32(data[offset : offset+4])
		commandSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if commandSize < 8 || offset+commandSize > loadEnd {
			return errors.New("release Mach-O load command size is invalid")
		}
		if command == machLCUUID {
			return errors.New("release Mach-O already contains LC_UUID")
		}
		offset += commandSize
	}
	if offset != loadEnd {
		return errors.New("release Mach-O load command table size is inconsistent")
	}
	for _, value := range data[loadEnd : loadEnd+machUUIDCmdSize] {
		if value != 0 {
			return errors.New("release Mach-O has no padding for LC_UUID")
		}
	}
	digest := sha256.Sum256(data)
	uuid := digest[:16]
	uuid[6] = (uuid[6] & 0x0f) | 0x50
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	binary.LittleEndian.PutUint32(data[loadEnd:loadEnd+4], machLCUUID)
	binary.LittleEndian.PutUint32(data[loadEnd+4:loadEnd+8], machUUIDCmdSize)
	copy(data[loadEnd+8:loadEnd+machUUIDCmdSize], uuid)
	binary.LittleEndian.PutUint32(data[16:20], ncmds+1)
	binary.LittleEndian.PutUint32(data[20:24], sizeofcmds+machUUIDCmdSize)
	return os.WriteFile(path, data, 0o755)
}
