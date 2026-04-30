package fax

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

func WriteCallFile(spoolPath string, jobID int, targetExtension int, tiffPath string, tempDir string, callerID string) (string, error) {
	content := fmt.Sprintf("Channel: PJSIP/%d\nCallerID: %s\nMaxRetries: 1\nRetryTime: 60\nWaitTime: 30\nApplication: SendFAX\nData: %s,d,f\n",
		targetExtension, callerID, tiffPath)

	callFileName := fmt.Sprintf("fax_%d.call", jobID)
	finalPath := filepath.Join(spoolPath, callFileName)

	// Write to app-owned temp dir first, then copy into spool.
	// Can't use rename across filesystems, so we write + copy + remove.
	tmpFile, err := os.CreateTemp(tempDir, ".fax_*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write call file: %w", err)
	}
	tmpFile.Close()

	// Copy into spool directory
	if err := copyFile(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("copy call file to spool: %w", err)
	}
	os.Remove(tmpPath)

	// Chown to asterisk user so Asterisk can set utime on the file
	if u, err := user.Lookup("asterisk"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		os.Chown(finalPath, uid, gid)
	}

	return callFileName, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
