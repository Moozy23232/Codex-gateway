package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

func releaseProject(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("release packaging runs on Linux with GNU tar and coreutils")
	}
	for _, tool := range []string{"bash", "go", "tar", "gzip", "sha256sum", "mktemp", "cp", "mv", "mkdir", "chmod", "rm", "sort"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release packaging requires %s: %v", tool, err)
		}
	}
	for tool, identity := range map[string]string{"tar": "GNU tar", "mv": "GNU coreutils"} {
		output, err := exec.Command(tool, "--version").CombinedOutput()
		if err != nil || !bytes.Contains(output, []byte(identity)) {
			t.Skipf("release packaging requires %s; %s is unavailable", identity, tool)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func releaseEnvironment(values map[string]string) []string {
	drop := map[string]bool{"VERSION": true, "TARGETS": true, "OUTPUT": true, "OUTPUT_DIR": true, "SOURCE_DATE_EPOCH": true, "GOOS": true, "GOARCH": true, "GOFLAGS": true, "GOTOOLCHAIN": true, "GOWORK": true}
	for key := range values {
		drop[key] = true
	}
	var result []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !drop[key] {
			result = append(result, item)
		}
	}
	result = append(result, "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=")
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func packageRelease(t *testing.T, root, destination string, values map[string]string) ([]byte, error) {
	t.Helper()
	environment := map[string]string{"VERSION": "v0.2.0-rc.1+build.7", "TARGETS": "linux/" + runtime.GOARCH, "OUTPUT_DIR": destination}
	for key, value := range values {
		environment[key] = value
	}
	command := exec.Command("bash", "scripts/package.sh")
	command.Dir = root
	command.Env = releaseEnvironment(environment)
	return command.CombinedOutput()
}

func requireMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected output at %s: %v", path, err)
	}
}

func requireVersion(t *testing.T, binary, version string) {
	t.Helper()
	command := exec.Command(binary, "--version")
	command.Env = []string{"PATH=/nonexistent"}
	output, err := command.CombinedOutput()
	if err != nil || string(output) != version+"\n" {
		t.Fatalf("binary version = %q (%v), want %q", output, err, version+"\n")
	}
}

func TestPackageNativeReleaseAndCurlInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("native build, reproducible archive, and curl installer integration")
	}
	root := releaseProject(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required for the release installation integration")
	}
	version := "0.2.0-rc.1+build.7"
	work := t.TempDir()
	first, second := filepath.Join(work, "first release"), filepath.Join(work, "second release")
	// buildvcs=false must keep source builds independent of a working Git command.
	toolDir := filepath.Join(work, "tools")
	if err := os.Mkdir(toolDir, 0700); err != nil {
		t.Fatal(err)
	}
	gitCalled := filepath.Join(work, "git-was-called")
	if err := os.WriteFile(filepath.Join(toolDir, "git"), []byte("#!/bin/sh\nprintf called > \"$RELEASE_TEST_GIT_MARKER\"\nexit 71\n"), 0755); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"PATH": toolDir + string(os.PathListSeparator) + os.Getenv("PATH"), "RELEASE_TEST_GIT_MARKER": gitCalled}
	for _, destination := range []string{first, second} {
		if output, err := packageRelease(t, root, destination, environment); err != nil {
			t.Fatalf("package: %v\n%s", err, output)
		}
	}
	requireMissing(t, gitCalled)
	archiveName := "codex-gateway_linux_" + runtime.GOARCH + ".tar.gz"
	names := []string{archiveName, "install.sh", "SHA256SUMS"}
	entries, err := os.ReadDir(first)
	if err != nil {
		t.Fatal(err)
	}
	var gotNames []string
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name())
	}
	sort.Strings(names)
	sort.Strings(gotNames)
	if strings.Join(gotNames, ",") != strings.Join(names, ",") {
		t.Fatalf("unexpected release assets: %v", gotNames)
	}
	for _, name := range []string{archiveName, "install.sh", "SHA256SUMS"} {
		one, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		two, err := os.ReadFile(filepath.Join(second, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(one, two) {
			t.Fatalf("asset is not reproducible across output paths: %s", name)
		}
	}
	verifyChecksums(t, first, []string{archiveName, "install.sh"})
	installer, err := os.ReadFile(filepath.Join(root, "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	packagedInstaller, err := os.ReadFile(filepath.Join(first, "install.sh"))
	if err != nil || !bytes.Equal(installer, packagedInstaller) {
		t.Fatal("release installer differs from the repository script")
	}
	binary := inspectReleaseArchive(t, filepath.Join(first, archiveName), filepath.Join(work, "extracted binary"))
	requireVersion(t, binary, version)

	var requestsMu sync.Mutex
	requests := map[string]int{}
	prefix := "/download/v" + version + "/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		if name != archiveName && name != "SHA256SUMS" && name != "install.sh" {
			http.NotFound(w, r)
			return
		}
		requestsMu.Lock()
		requests[name]++
		requestsMu.Unlock()
		http.ServeFile(w, r, filepath.Join(first, name))
	}))
	defer server.Close()
	installDir := filepath.Join(work, "installed tools with spaces")
	if err := os.Mkdir(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	const originalLauncher = "#!/bin/sh\n[ \"$1\" = --version ] || exit 13\nprintf '%s\\n' 'fixture-native-codex'\n"
	if err := os.WriteFile(filepath.Join(installDir, "codex"), []byte(originalLauncher), 0755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-o", "pipefail", "-c",
		`curl --fail --silent --show-error --noproxy '*' "$1" | bash -s -- --version "$2" --install-dir "$3"`,
		"release-install-test", server.URL+prefix+"install.sh", "v"+version, installDir)
	command.Env = releaseEnvironment(map[string]string{
		"CODEX_GATEWAY_RELEASE_BASE_URL": server.URL,
		"NO_PROXY":                       "*", "no_proxy": "*",
	})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("curl installer against real package: %v\n%s", err, output)
	}
	requireVersion(t, filepath.Join(installDir, "codex-gateway"), version)
	target, err := os.Readlink(filepath.Join(installDir, "codex"))
	if err != nil || target != "codex-gateway" {
		t.Fatalf("ordinary codex entry was not installed: %q (%v)", target, err)
	}
	var manifest struct {
		Version            int    `json:"version"`
		OriginalExecutable string `json:"original_executable"`
	}
	rawManifest, err := os.ReadFile(filepath.Join(installDir, "codex-gateway.install.json"))
	if err != nil || json.Unmarshal(rawManifest, &manifest) != nil || manifest.Version != 1 || !filepath.IsAbs(manifest.OriginalExecutable) {
		t.Fatalf("original launcher manifest is invalid: %q (%v)", rawManifest, err)
	}
	preserved, err := os.ReadFile(manifest.OriginalExecutable)
	if err != nil || string(preserved) != originalLauncher {
		t.Fatalf("installer did not preserve the existing launcher: %q (%v)", preserved, err)
	}
	launcher := exec.Command(filepath.Join(installDir, "codex"), "--version")
	launcher.Env = []string{"PATH=/nonexistent", "HOME=" + filepath.Join(work, "isolated home"), "CODEX_HOME=" + filepath.Join(work, "isolated codex"), "CODEX_GATEWAY_HOME=" + filepath.Join(work, "isolated gateway")}
	if output, err := launcher.CombinedOutput(); err != nil || string(output) != "fixture-native-codex\n" {
		t.Fatalf("installed codex entry did not preserve the native launcher: %q (%v)", output, err)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	for _, name := range names {
		if requests[name] != 1 {
			t.Errorf("asset %s fetched %d times, want once", name, requests[name])
		}
	}
}

func verifyChecksums(t *testing.T, directory string, expected []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || seen[fields[1]] || filepath.Base(fields[1]) != fields[1] {
			t.Fatalf("invalid checksum line: %q", line)
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size {
			t.Fatalf("invalid SHA256 digest: %q", fields[0])
		}
		data, err := os.ReadFile(filepath.Join(directory, fields[1]))
		if err != nil {
			t.Fatal(err)
		}
		actual := sha256.Sum256(data)
		if !bytes.Equal(digest, actual[:]) {
			t.Errorf("checksum mismatch: %s", fields[1])
		}
		seen[fields[1]] = true
	}
	if len(seen) != len(expected) {
		t.Fatalf("unexpected checksum entries: %v", seen)
	}
	for _, name := range expected {
		if !seen[name] {
			t.Errorf("checksum missing for %s", name)
		}
	}
}

func inspectReleaseArchive(t *testing.T, archive, output string) string {
	t.Helper()
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 10 || !bytes.Equal(raw[4:8], []byte{0, 0, 0, 0}) || raw[3]&8 != 0 {
		t.Fatal("gzip header contains a source timestamp or filename")
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	archiveReader := tar.NewReader(reader)
	header, err := archiveReader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "codex-gateway" || header.Typeflag != tar.TypeReg || header.Mode != 0755 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.ModTime.Unix() != 0 {
		t.Fatalf("noncanonical archive header: %+v", header)
	}
	binary, err := io.ReadAll(archiveReader)
	if err != nil || len(binary) == 0 {
		t.Fatalf("empty or unreadable binary: %v", err)
	}
	if _, err := archiveReader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("archive contains more than its single root binary: %v", err)
	}
	if err := os.WriteFile(output, binary, 0755); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestBuildDefaultDevelopmentVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("native source build")
	}
	root := releaseProject(t)
	binary := filepath.Join(t.TempDir(), "codex-gateway")
	command := exec.Command("bash", "scripts/build.sh")
	command.Dir = root
	command.Env = releaseEnvironment(map[string]string{"OUTPUT": binary, "GOOS": "linux", "GOARCH": runtime.GOARCH})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("default build: %v\n%s", err, output)
	}
	requireVersion(t, binary, "0.2.0-dev")
}

func TestPackageRejectsInvalidVersionsAndTargets(t *testing.T) {
	root := releaseProject(t)
	for _, version := range []string{"", "latest", "v", "vv1.2.3", "1.2", "01.2.3", "1.02.3", "1.2.03", "1.2.3-01", "1.2.3-rc.01", "1.2.3+", "1.2.3-", "1.2.3/path", "1.2.3 evil", "1.2.3\n", "1.2.3-café", "1.2.3; touch forbidden", "$(touch forbidden)", "`touch forbidden`"} {
		t.Run("version="+version, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "release")
			if log, err := packageRelease(t, root, output, map[string]string{"VERSION": version}); err == nil {
				t.Fatalf("invalid version accepted: %q\n%s", version, log)
			}
			requireMissing(t, output)
		})
	}
	for _, targets := range []string{"", " \n ", "linux/amd64 linux/amd64", "windows/amd64", "linux/386", "darwin/x86_64", "linux/amd64 ../../elsewhere", "linux/*"} {
		t.Run("targets="+targets, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "release")
			if log, err := packageRelease(t, root, output, map[string]string{"TARGETS": targets}); err == nil {
				t.Fatalf("invalid targets accepted: %q\n%s", targets, log)
			}
			requireMissing(t, output)
		})
	}
	output := filepath.Join(t.TempDir(), "release")
	command := exec.Command("bash", "scripts/package.sh")
	command.Dir = root
	command.Env = releaseEnvironment(map[string]string{"OUTPUT_DIR": output})
	if log, err := command.CombinedOutput(); err == nil || !bytes.Contains(log, []byte("Set VERSION explicitly")) {
		t.Fatalf("omitted version was not rejected: %v %s", err, log)
	}
	requireMissing(t, output)
}

func TestPackageRefusesExistingOutput(t *testing.T) {
	root := releaseProject(t)
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprintf("empty=%v", empty), func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "release")
			if err := os.Mkdir(directory, 0755); err != nil {
				t.Fatal(err)
			}
			if !empty {
				if err := os.WriteFile(filepath.Join(directory, "previous-release"), []byte("keep this release\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if log, err := packageRelease(t, root, directory, nil); err == nil || !bytes.Contains(log, []byte("refusing to overwrite")) {
				t.Fatalf("existing directory was not refused: %v %s", err, log)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if empty && len(entries) != 0 {
				t.Fatal("empty existing directory was modified")
			}
			if !empty {
				contents, err := os.ReadFile(filepath.Join(directory, "previous-release"))
				if err != nil || string(contents) != "keep this release\n" || len(entries) != 1 {
					t.Fatal("historical release was modified")
				}
			}
		})
	}
}

func TestPackageFailurePreservesDiagnostics(t *testing.T) {
	root := releaseProject(t)
	work := t.TempDir()
	toolDir := filepath.Join(work, "tools")
	if err := os.Mkdir(toolDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "go"), []byte("#!/bin/sh\nprintf 'compiler failed for the test\\n' >&2\nexit 23\n"), 0755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(work, "release")
	log, err := packageRelease(t, root, destination, map[string]string{"PATH": toolDir + string(os.PathListSeparator) + os.Getenv("PATH")})
	if err == nil || !bytes.Contains(log, []byte("diagnostic files retained at:")) {
		t.Fatalf("failed build did not retain diagnostics: %v %s", err, log)
	}
	requireMissing(t, destination)
	stages, err := filepath.Glob(filepath.Join(work, ".codex-gateway-package.*"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("expected one retained staging directory, got %v (%v)", stages, err)
	}
	status, err := os.ReadFile(filepath.Join(stages[0], "exit-status"))
	if err != nil || string(status) != "23\n" {
		t.Fatalf("failed compiler status was lost: %q (%v)", status, err)
	}
	buildLog, err := os.ReadFile(filepath.Join(stages[0], "builds", "linux_"+runtime.GOARCH, "build.log"))
	if err != nil || !bytes.Contains(buildLog, []byte("compiler failed for the test")) {
		t.Fatalf("failed compiler log was lost: %q (%v)", buildLog, err)
	}
}

func TestPackageRefusesOutputCreatedDuringBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("native package with a publication race")
	}
	root := releaseProject(t)
	work := t.TempDir()
	toolDir := filepath.Join(work, "tools")
	if err := os.Mkdir(toolDir, 0700); err != nil {
		t.Fatal(err)
	}
	mv, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env bash\nset -eu\nif [[ $1 != --version ]]; then\n  destination=\"${@: -1}\"\n  mkdir -- \"$destination\"\n  printf 'another release\\n' > \"$destination/keep\"\nfi\nexec \"$RELEASE_TEST_MV\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(toolDir, "mv"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(work, "release")
	log, err := packageRelease(t, root, destination, map[string]string{"PATH": toolDir + string(os.PathListSeparator) + os.Getenv("PATH"), "RELEASE_TEST_MV": mv})
	if err == nil || !bytes.Contains(log, []byte("Output appeared during packaging")) {
		t.Fatalf("publication race was not refused: %v %s", err, log)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "keep"))
	if err != nil || string(contents) != "another release\n" {
		t.Fatalf("concurrent release was overwritten: %v %q", err, contents)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 {
		t.Fatalf("concurrent release was changed: %v %v", entries, err)
	}
	stages, err := filepath.Glob(filepath.Join(work, ".codex-gateway-package.*"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("failed publication diagnostics were not retained: %v %v", stages, err)
	}
	if _, err := os.Stat(filepath.Join(stages[0], "assets", "SHA256SUMS")); err != nil {
		t.Fatalf("completed package was not retained for diagnosis: %v", err)
	}
}
