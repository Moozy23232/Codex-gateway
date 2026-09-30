//go:build linux || darwin

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// InstallGateway is called by the verified downloaded executable. Keeping the
// final installation in Go lets paths remain data and gives the three installed
// entries one rollback path without requiring a scripting-language runtime.
func InstallGateway(installDir string) error {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupted)
	staging, err := os.Executable()
	if err != nil {
		return errors.New("cannot locate the downloaded executable")
	}
	return installGatewayBinary(staging, installDir, func(source, destination string) error {
		select {
		case <-interrupted:
			return errors.New("installation interrupted")
		default:
			return os.Rename(source, destination)
		}
	})
}

type installEntry struct {
	path   string
	stage  string
	backup string
	before os.FileInfo
	new    os.FileInfo
	done   bool
}

func installGatewayBinary(staging, installDir string, replace func(string, string) error) (result error) {
	if installDir == "" || strings.ContainsAny(installDir, "\x00\r\n") {
		return errors.New("installation directory must be nonempty and contain no line breaks")
	}
	installDir, err := filepath.Abs(installDir)
	if err != nil {
		return err
	}
	staging, err = filepath.Abs(staging)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return fmt.Errorf("cannot create installation directory: %w", err)
	}
	lock, err := lifecycleOpen(filepath.Join(installDir, ".codex-gateway-install.lock"), syscall.O_RDWR|syscall.O_CREAT, false)
	if err != nil {
		return fmt.Errorf("cannot lock installation directory: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another installation is using this directory; retry after it finishes")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	binaryPath := filepath.Join(installDir, "codex-gateway")
	aliasPath := filepath.Join(installDir, "codex")
	manifestPath := filepath.Join(installDir, installManifestName)
	if staging == binaryPath || staging == aliasPath || staging == manifestPath {
		return errors.New("installation requires a separate downloaded staging executable")
	}
	stagedInfo, err := os.Lstat(staging)
	if err != nil || !stagedInfo.Mode().IsRegular() || stagedInfo.Mode().Perm()&0111 == 0 {
		return errors.New("downloaded staging executable must be a regular executable file")
	}
	entries := []*installEntry{{path: manifestPath}, {path: binaryPath, stage: staging}, {path: aliasPath}}
	for _, entry := range entries {
		entry.before, err = installExistingFile(entry.path)
		if err != nil {
			return err
		}
		if entry.before != nil && os.SameFile(entry.before, stagedInfo) {
			return errors.New("downloaded executable must be separate from the current installation")
		}
	}
	manifest := installManifest{Version: 1}
	if entries[0].before != nil {
		previous, err := readInstallManifest(installDir)
		if err != nil || previous == nil {
			return errors.New("existing installation manifest is invalid; refusing to replace it")
		}
		manifest = *previous
		for _, path := range []string{manifest.OriginalExecutable, manifest.NativeExecutable} {
			if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) == aliasPath || filepath.Clean(path) == binaryPath) {
				return errors.New("existing installation manifest contains an invalid executable path")
			}
		}
	}
	managedAlias := false
	if entries[2].before != nil {
		target, linkErr := os.Readlink(aliasPath)
		managedAlias = linkErr == nil && target == "codex-gateway"
		if managedAlias && entries[0].before == nil {
			return errors.New("codex already points to codex-gateway without an installation manifest; refusing to lose the original launcher")
		}
		if !managedAlias {
			if resolved, statErr := os.Stat(aliasPath); statErr == nil && entries[1].before != nil {
				installed, installedErr := os.Stat(binaryPath)
				if installedErr == nil && os.SameFile(resolved, installed) {
					return errors.New("codex points to codex-gateway through an unmanaged entry; refusing to preserve a recursive launcher")
				}
			}
		}
	}

	var original string
	var temporary []string
	keepRecovery := false
	defer func() {
		for _, path := range temporary {
			_ = os.Remove(path)
		}
		if !keepRecovery {
			for _, entry := range entries {
				if entry.backup != "" {
					_ = os.Remove(entry.backup)
				}
			}
			if result != nil && original != "" {
				_ = os.Remove(original)
			}
		}
	}()
	if entries[2].before != nil && !managedAlias {
		original, err = installReserveLink(aliasPath, installDir, "codex-gateway-original-")
		if err != nil {
			return fmt.Errorf("cannot preserve the existing codex launcher: %w", err)
		}
		manifest.OriginalExecutable = original
		// A newly replaced user launcher may point at a different installation.
		manifest.NativeExecutable = ""
	}
	for _, entry := range entries {
		if entry.before != nil {
			entry.backup, err = installReserveLink(entry.path, installDir, ".codex-gateway-rollback-")
			if err != nil {
				return fmt.Errorf("cannot preserve the current installation: %w", err)
			}
		}
	}
	manifestFile, err := os.CreateTemp(installDir, ".codex-gateway-manifest-")
	if err != nil {
		return err
	}
	entries[0].stage = manifestFile.Name()
	temporary = append(temporary, manifestFile.Name())
	encoder := json.NewEncoder(manifestFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		manifestFile.Close()
		return err
	}
	if err := manifestFile.Sync(); err != nil {
		manifestFile.Close()
		return err
	}
	if err := manifestFile.Close(); err != nil {
		return err
	}
	aliasStage, err := installReserveName(installDir, ".codex-gateway-alias-")
	if err != nil {
		return err
	}
	if err := os.Symlink("codex-gateway", aliasStage); err != nil {
		return err
	}
	entries[2].stage = aliasStage
	temporary = append(temporary, aliasStage)

	for _, entry := range entries {
		current, statErr := os.Lstat(entry.path)
		unchanged := entry.before == nil && errors.Is(statErr, os.ErrNotExist)
		if entry.before != nil {
			unchanged = statErr == nil && os.SameFile(entry.before, current)
		}
		if !unchanged {
			result = errors.New("installation target changed during setup; refusing to replace it")
			break
		}
		entry.new, err = os.Lstat(entry.stage)
		if err != nil {
			result = err
			break
		}
		if err := replace(entry.stage, entry.path); err != nil {
			result = fmt.Errorf("cannot install %s: %w", filepath.Base(entry.path), err)
			break
		}
		entry.done = true
	}
	if result == nil {
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !entry.done {
			continue
		}
		current, err := os.Lstat(entry.path)
		if err != nil || !os.SameFile(current, entry.new) {
			keepRecovery = true
			result = errors.Join(result, fmt.Errorf("%s changed during rollback; its previous entry is retained at %s", entry.path, entry.backup))
			continue
		}
		if entry.backup != "" {
			err = os.Rename(entry.backup, entry.path)
		} else {
			err = os.Remove(entry.path)
		}
		if err != nil {
			keepRecovery = true
			result = errors.Join(result, fmt.Errorf("could not restore %s; previous entry retained at %s: %w", entry.path, entry.backup, err))
		}
	}
	return result
}

func installExistingFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		if filepath.Base(path) == "codex" && info.Mode().Perm()&0111 == 0 {
			return nil, errors.New("existing codex entry is not executable; refusing to replace it")
		}
		return info, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Stat(path); err == nil && target.IsDir() {
			return nil, fmt.Errorf("installation target is a directory: %s", path)
		}
		return info, nil
	}
	return nil, fmt.Errorf("installation target is not a regular file or symlink: %s", path)
}

func installReserveName(directory, prefix string) (string, error) {
	file, err := os.CreateTemp(directory, prefix)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func installReserveLink(source, directory, prefix string) (string, error) {
	name, err := installReserveName(directory, prefix)
	if err != nil {
		return "", err
	}
	// Link creates a new entry exclusively and preserves relative symlinks by
	// keeping the saved launcher in its original directory.
	if err := os.Link(source, name); err != nil {
		return "", err
	}
	return name, nil
}
