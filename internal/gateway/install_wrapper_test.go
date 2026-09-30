//go:build linux || darwin

package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func installerStaging(t *testing.T, directory, contents string) string {
	t.Helper()
	file, err := os.CreateTemp(directory, ".codex-gateway.fixture-*")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if _, err := file.WriteString(contents); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Chmod(0755); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func requireInstallAlias(t *testing.T, directory string) installManifest {
	t.Helper()
	target, err := os.Readlink(filepath.Join(directory, "codex"))
	if err != nil || target != "codex-gateway" {
		t.Fatalf("codex alias = %q (%v)", target, err)
	}
	var manifest installManifest
	if err := readJSON(filepath.Join(directory, installManifestName), &manifest, true); err != nil || manifest.Version != 1 {
		t.Fatalf("installation manifest: %+v (%v)", manifest, err)
	}
	return manifest
}

func requireFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != expected {
		t.Fatalf("%s changed: %q (%v)", filepath.Base(path), contents, err)
	}
}

func requireNoInstallTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		for _, prefix := range []string{".codex-gateway-rollback-", ".codex-gateway-alias-", ".codex-gateway-manifest-"} {
			if strings.HasPrefix(entry.Name(), prefix) {
				t.Errorf("installation temporary file remains: %s", entry.Name())
			}
		}
	}
}

func TestInstallGatewayWithoutExistingCodex(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "tools with spaces 'quotes' $dollar `literal`")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	staging := installerStaging(t, directory, "new gateway")
	if err := installGatewayBinary(staging, directory, os.Rename); err != nil {
		t.Fatal(err)
	}
	manifest := requireInstallAlias(t, directory)
	if manifest.OriginalExecutable != "" || manifest.NativeExecutable != "" {
		t.Fatal("installation invented a Codex executable")
	}
	requireFileContents(t, filepath.Join(directory, "codex-gateway"), "new gateway")
	info, err := os.Stat(filepath.Join(directory, installManifestName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("installation manifest must be private")
	}
	requireNoInstallTemporaryFiles(t, directory)
}

func TestInstallGatewayPreservesLauncherAndStableUpgrade(t *testing.T) {
	for _, symbolic := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%v", symbolic), func(t *testing.T) {
			directory := t.TempDir()
			alias := filepath.Join(directory, "codex")
			const launcher = "#!/bin/sh\n# keep existing backup/startup chain intact\nexit 0\n"
			if symbolic {
				if err := os.WriteFile(filepath.Join(directory, "native codex"), []byte(launcher), 0751); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("native codex", alias); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(alias, []byte(launcher), 0751); err != nil {
				t.Fatal(err)
			}
			unknownBackup := filepath.Join(directory, "codex-gateway-original-existing")
			if err := os.WriteFile(unknownBackup, []byte("user-owned backup"), 0600); err != nil {
				t.Fatal(err)
			}
			var original string
			for index := range 2 {
				staging := installerStaging(t, directory, fmt.Sprintf("gateway %d", index))
				if err := installGatewayBinary(staging, directory, os.Rename); err != nil {
					t.Fatal(err)
				}
				manifest := requireInstallAlias(t, directory)
				if original != "" && manifest.OriginalExecutable != original {
					t.Fatal("upgrade wrapped its own codex alias")
				}
				original = manifest.OriginalExecutable
				if !filepath.IsAbs(original) || filepath.Dir(original) != directory || original == alias {
					t.Fatalf("original launcher was not saved beside codex: %q", original)
				}
				requireFileContents(t, original, launcher)
				info, err := os.Stat(original)
				if err != nil || info.Mode().Perm() != 0751 {
					t.Fatal("original launcher permissions changed")
				}
				if symbolic {
					target, err := os.Readlink(original)
					if err != nil || target != "native codex" {
						t.Fatal("original relative symlink changed")
					}
				}
				requireFileContents(t, unknownBackup, "user-owned backup")
				requireNoInstallTemporaryFiles(t, directory)
			}
			backups, _ := filepath.Glob(filepath.Join(directory, "codex-gateway-original-*"))
			if len(backups) != 2 {
				t.Fatalf("reinstall created duplicate launcher backups: %v", backups)
			}
		})
	}
}

func TestInstallGatewayPreservesNewLauncherAfterNativeReinstall(t *testing.T) {
	directory := t.TempDir()
	alias := filepath.Join(directory, "codex")
	if err := os.WriteFile(alias, []byte("old native"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installGatewayBinary(installerStaging(t, directory, "gateway one"), directory, os.Rename); err != nil {
		t.Fatal(err)
	}
	first := requireInstallAlias(t, directory)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, []byte("updated native"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installGatewayBinary(installerStaging(t, directory, "gateway two"), directory, os.Rename); err != nil {
		t.Fatal(err)
	}
	second := requireInstallAlias(t, directory)
	if first.OriginalExecutable == second.OriginalExecutable {
		t.Fatal("new native launcher was not preserved separately")
	}
	requireFileContents(t, first.OriginalExecutable, "old native")
	requireFileContents(t, second.OriginalExecutable, "updated native")
}

func TestInstallGatewayPreservesBrokenNativeSymlink(t *testing.T) {
	directory := t.TempDir()
	if err := os.Symlink("native-not-installed-yet", filepath.Join(directory, "codex")); err != nil {
		t.Fatal(err)
	}
	if err := installGatewayBinary(installerStaging(t, directory, "new gateway"), directory, os.Rename); err != nil {
		t.Fatal(err)
	}
	manifest := requireInstallAlias(t, directory)
	target, err := os.Readlink(manifest.OriginalExecutable)
	if err != nil || target != "native-not-installed-yet" {
		t.Fatal("broken original symlink was discarded")
	}
}

func TestInstallGatewayRollsBackEveryPublicationFailure(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for failAt := 1; failAt <= 3; failAt++ {
			t.Run(fmt.Sprintf("existing=%v/step=%d", existing, failAt), func(t *testing.T) {
				directory := t.TempDir()
				old := map[string]string{"codex-gateway": "previous gateway", "codex": "previous launcher", installManifestName: "{\"version\":1}\n"}
				if existing {
					for name, contents := range old {
						if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0750); err != nil {
							t.Fatal(err)
						}
					}
				}
				calls := 0
				err := installGatewayBinary(installerStaging(t, directory, "new gateway"), directory, func(from, to string) error {
					calls++
					if calls == failAt {
						return errors.New("injected disk/rename failure")
					}
					return os.Rename(from, to)
				})
				if err == nil || !strings.Contains(err.Error(), "injected disk/rename failure") {
					t.Fatalf("publication failure not reported: %v", err)
				}
				for name, contents := range old {
					path := filepath.Join(directory, name)
					if existing {
						requireFileContents(t, path, contents)
						info, err := os.Stat(path)
						if err != nil || info.Mode().Perm() != 0750 {
							t.Fatalf("rollback changed permissions of %s", name)
						}
					} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("partial new installation remains: %s (%v)", name, err)
					}
				}
				backups, _ := filepath.Glob(filepath.Join(directory, "codex-gateway-original-*"))
				if len(backups) != 0 {
					t.Fatalf("failed installation left unused launcher backups: %v", backups)
				}
				requireNoInstallTemporaryFiles(t, directory)
			})
		}
	}
}

func TestInstallGatewayRejectsUnsafeEntries(t *testing.T) {
	for _, name := range []string{"codex directory", "manifest symlink", "invalid manifest", "unmanaged gateway alias", "nonexecutable codex"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			binary := filepath.Join(directory, "codex-gateway")
			alias := filepath.Join(directory, "codex")
			if err := os.WriteFile(binary, []byte("previous gateway"), 0755); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "codex directory":
				if err := os.Mkdir(alias, 0755); err != nil {
					t.Fatal(err)
				}
			case "manifest symlink":
				if err := os.Symlink(binary, filepath.Join(directory, installManifestName)); err != nil {
					t.Fatal(err)
				}
			case "invalid manifest":
				if err := os.WriteFile(filepath.Join(directory, installManifestName), []byte("user-owned invalid metadata"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unmanaged gateway alias":
				if err := os.Symlink("codex-gateway", alias); err != nil {
					t.Fatal(err)
				}
			case "nonexecutable codex":
				if err := os.WriteFile(alias, []byte("user-owned text"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := installGatewayBinary(installerStaging(t, directory, "new gateway"), directory, os.Rename); err == nil {
				t.Fatal("unsafe existing entry was replaced")
			}
			requireFileContents(t, binary, "previous gateway")
			requireNoInstallTemporaryFiles(t, directory)
		})
	}
}

func TestInstallGatewayRespectsConcurrentInstallLock(t *testing.T) {
	directory := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(directory, ".codex-gateway-install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	err = installGatewayBinary(installerStaging(t, directory, "new gateway"), directory, os.Rename)
	if err == nil || !strings.Contains(err.Error(), "another installation") {
		t.Fatalf("concurrent installation lock ignored: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "codex-gateway")); !os.IsNotExist(err) {
		t.Fatal("concurrent install changed the binary")
	}
}
