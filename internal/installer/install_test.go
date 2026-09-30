package installer_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type releaseServer struct {
	server       *httptest.Server
	version      string
	asset        string
	archive      []byte
	checksums    string
	latestTarget string
	status       map[string]int
	mu           sync.Mutex
	requests     map[string]int
}

func archiveWithEntries(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	writer := tar.NewWriter(gzipWriter)
	for name, content := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func versionExecutable(version string) string {
	return "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '%s\\n' '" + version + "'\n"
}

func newRelease(t *testing.T, version, platform, architecture string) *releaseServer {
	t.Helper()
	r := &releaseServer{
		version:  version,
		asset:    fmt.Sprintf("codex-gateway_%s_%s.tar.gz", platform, architecture),
		status:   make(map[string]int),
		requests: make(map[string]int),
	}
	r.setArchive(archiveWithEntries(t, map[string]string{"codex-gateway": versionExecutable(version)}))
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.requests[req.Method+" "+req.URL.Path]++
		count := r.requests[req.Method+" "+req.URL.Path]
		if code := r.status[req.URL.Path]; code != 0 {
			http.Error(w, "fixture release unavailable", code)
			return
		}
		switch req.URL.Path {
		case "/latest":
			target := r.latestTarget
			if target == "" {
				target = "/tag/v" + version
				// A second latest lookup observes a different release. A successful
				// install must use the tag resolved by its single first lookup.
				if count > 1 {
					target = "/tag/v99.0.0"
				}
			}
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusFound)
		case "/download/v" + version + "/SHA256SUMS":
			io.WriteString(w, r.checksums)
		case "/download/v" + version + "/" + r.asset:
			w.Write(r.archive)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *releaseServer) setArchive(archive []byte) {
	// Configure before starting the server, or while holding mu.
	r.archive = archive
	r.checksums = fmt.Sprintf("%x  %s\n%s  install.sh\n", sha256.Sum256(archive), r.asset, strings.Repeat("0", 64))
}

func (r *releaseServer) count(method, path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[method+" "+path]
}

func (r *releaseServer) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, count := range r.requests {
		total += count
	}
	return total
}

type installation struct {
	t       *testing.T
	root    string
	home    string
	temp    string
	tools   string
	script  string
	release *releaseServer
	values  map[string]string
}

func newInstallation(t *testing.T, release *releaseServer, system, machine, checksumTool string) *installation {
	t.Helper()
	i := &installation{t: t, root: t.TempDir(), release: release, values: make(map[string]string)}
	i.home, i.temp, i.tools = filepath.Join(i.root, "home"), filepath.Join(i.root, "tmp"), filepath.Join(i.root, "tools")
	for _, directory := range []string{i.home, i.temp, i.tools} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	i.script, err = filepath.Abs("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The installer receives a minimal PATH containing real system utilities,
	// with no Go, Python, jq, inherited credentials or user configuration.
	for _, name := range []string{"curl", "tar", "gzip", "awk", "mktemp", "mkdir", "mv", "chmod", "rm", checksumTool} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("installer test requires %s: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(i.tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	uname := "#!/bin/sh\ncase \"$1\" in\n-s) printf '%s\\n' '" + system + "';;\n-m) printf '%s\\n' '" + machine + "';;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(filepath.Join(i.tools, "uname"), []byte(uname), 0700); err != nil {
		t.Fatal(err)
	}
	i.values["HOME"], i.values["TMPDIR"], i.values["PATH"] = i.home, i.temp, i.tools
	i.values["CODEX_GATEWAY_RELEASE_BASE_URL"] = release.server.URL
	return i
}

func (i *installation) run(args ...string) (string, error) {
	i.t.Helper()
	command := exec.Command("/bin/sh", append([]string{i.script}, args...)...)
	command.Dir = i.root
	for key, value := range i.values {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func (i *installation) defaultDirectory() string {
	return filepath.Join(i.home, ".local", "bin")
}

func (i *installation) assertClean() {
	i.t.Helper()
	entries, err := os.ReadDir(i.temp)
	if err != nil || len(entries) != 0 {
		i.t.Fatalf("temporary files remain: entries=%v error=%v", entries, err)
	}
	if err := filepath.WalkDir(i.home, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasPrefix(entry.Name(), ".codex-gateway.") {
			i.t.Errorf("staging file remains: %s", path)
		}
		return nil
	}); err != nil {
		i.t.Fatal(err)
	}
}

func assertVersion(t *testing.T, directory, version string) {
	t.Helper()
	binary := filepath.Join(directory, "codex-gateway")
	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || string(output) != version+"\n" {
		t.Fatalf("installed version: %v, %q", err, output)
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0755 {
		t.Fatalf("installed executable mode: %v, %v", info, err)
	}
}

func TestInstallLatestResolvesOneTag(t *testing.T) {
	release := newRelease(t, "1.2.3", "linux", "amd64")
	install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
	output, err := install.run()
	if err != nil {
		t.Fatalf("install: %v\n%s", err, output)
	}
	assertVersion(t, install.defaultDirectory(), release.version)
	if release.count("HEAD", "/latest") != 1 || release.total() != 3 {
		t.Fatalf("latest was not resolved exactly once: %d total requests", release.total())
	}
	if !strings.Contains(output, install.defaultDirectory()) || !strings.Contains(output, "PATH") {
		t.Fatalf("installation location/PATH hint missing: %s", output)
	}
	install.assertClean()
}

func TestInstallDirectoriesAndVersionOptions(t *testing.T) {
	tests := []struct {
		name, version, versionArg string
		environment, flag, inPath bool
	}{
		{name: "explicit version without prefix", version: "1.2.3", versionArg: "1.2.3"},
		{name: "version with prefix", version: "1.2.3-rc.1+build.01", versionArg: "v1.2.3-rc.1+build.01"},
		{name: "environment directory with spaces", version: "0.0.0", environment: true},
		{name: "flags override environment", version: "1.2.3", versionArg: "v1.2.3", environment: true, flag: true},
		{name: "directory already on PATH", version: "1.2.3", versionArg: "1.2.3", flag: true, inPath: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			release := newRelease(t, test.version, "linux", "amd64")
			install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
			directory := install.defaultDirectory()
			var args []string
			if test.environment {
				directory = filepath.Join(install.home, "env programs", "bin")
				install.values["CODEX_GATEWAY_INSTALL_DIR"] = directory
				install.values["CODEX_GATEWAY_VERSION"] = "v" + test.version
			}
			if test.versionArg != "" {
				args = append(args, "--version", test.versionArg)
				if test.environment {
					install.values["CODEX_GATEWAY_VERSION"] = "invalid-env-version"
				}
			}
			if test.flag {
				directory = filepath.Join(install.home, "flag programs", "bin")
				args = append(args, "--install-dir", directory)
			}
			if test.inPath {
				install.values["PATH"] = directory + ":" + install.tools
			}
			output, err := install.run(args...)
			if err != nil {
				t.Fatalf("install: %v\n%s", err, output)
			}
			assertVersion(t, directory, test.version)
			if release.count("HEAD", "/latest") != 0 {
				t.Fatal("explicit version performed a latest lookup")
			}
			if test.inPath && strings.Contains(output, "Add ") {
				t.Fatalf("unexpected PATH hint: %s", output)
			}
			if test.environment && test.flag {
				if _, err := os.Stat(install.values["CODEX_GATEWAY_INSTALL_DIR"]); !os.IsNotExist(err) {
					t.Fatalf("environment directory should not be created: %v", err)
				}
			}
			install.assertClean()
		})
	}
}

func TestInstallPlatformMappingsAndShasum(t *testing.T) {
	for _, test := range []struct{ system, machine, platform, architecture, checksum string }{
		{"Linux", "aarch64", "linux", "arm64", "sha256sum"},
		{"Linux", "arm64", "linux", "arm64", "sha256sum"},
		{"Darwin", "x86_64", "darwin", "amd64", "shasum"},
		{"Darwin", "arm64", "darwin", "arm64", "shasum"},
	} {
		t.Run(test.system+" "+test.machine, func(t *testing.T) {
			release := newRelease(t, "1.2.3", test.platform, test.architecture)
			install := newInstallation(t, release, test.system, test.machine, test.checksum)
			output, err := install.run("--version=1.2.3")
			if err != nil {
				t.Fatalf("install: %v\n%s", err, output)
			}
			assertVersion(t, install.defaultDirectory(), "1.2.3")
			install.assertClean()
		})
	}
}

func TestInstallFailurePreservesExistingBinary(t *testing.T) {
	for _, name := range []string{"missing release", "checksum HTTP error", "archive HTTP error", "missing checksum", "duplicate checksum", "invalid checksum", "wrong checksum", "wrong binary version", "binary cannot execute", "missing archive member"} {
		t.Run(name, func(t *testing.T) {
			release := newRelease(t, "1.2.3", "linux", "amd64")
			install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
			directory := install.defaultDirectory()
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(directory, "codex-gateway")
			previous := []byte(versionExecutable("0.9.0"))
			if err := os.WriteFile(destination, previous, 0700); err != nil {
				t.Fatal(err)
			}
			release.mu.Lock()
			switch name {
			case "missing release":
				release.status["/latest"] = http.StatusNotFound
			case "checksum HTTP error":
				release.status["/download/v1.2.3/SHA256SUMS"] = http.StatusNotFound
			case "archive HTTP error":
				release.status["/download/v1.2.3/"+release.asset] = http.StatusBadGateway
			case "missing checksum":
				release.checksums = strings.Repeat("0", 64) + "  install.sh\n"
			case "duplicate checksum":
				release.checksums += release.checksums
			case "invalid checksum":
				release.checksums = "invalid  " + release.asset + "\n"
			case "wrong checksum":
				release.checksums = strings.Repeat("0", 64) + "  " + release.asset + "\n"
			case "wrong binary version":
				release.setArchive(archiveWithEntries(t, map[string]string{"codex-gateway": versionExecutable("9.9.9")}))
			case "binary cannot execute":
				release.setArchive(archiveWithEntries(t, map[string]string{"codex-gateway": "#!/bin/sh\nexit 17\n"}))
			case "missing archive member":
				release.setArchive(archiveWithEntries(t, map[string]string{"different-file": "not the gateway"}))
			}
			release.mu.Unlock()
			output, err := install.run()
			if err == nil {
				t.Fatalf("expected installation failure: %s", output)
			}
			contents, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(contents, previous) {
				t.Fatalf("old binary changed: %q, %v", contents, err)
			}
			info, err := os.Stat(destination)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("old binary mode changed: %v, %v", info, err)
			}
			install.assertClean()
		})
	}
}

func TestInstallUpgradeTouchesOnlySelectedBinary(t *testing.T) {
	release := newRelease(t, "1.2.3", "linux", "amd64")
	install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
	install.values["TAR_OPTIONS"] = "--this-option-must-not-be-used"
	directory := filepath.Join(install.home, "custom bin")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "codex-gateway"), []byte(versionExecutable("0.9.0")), 0700); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(directory, "other-tool"), filepath.Join(install.home, ".zshrc"), filepath.Join(install.home, ".bashrc"), filepath.Join(install.home, ".codex", "config.toml"), filepath.Join(install.home, ".config", "codex-gateway", "config.json")}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("fixture must stay unchanged\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		output, err := install.run("--version", "1.2.3", "--install-dir="+directory)
		if err != nil {
			t.Fatalf("install/reinstall: %v\n%s", err, output)
		}
		assertVersion(t, directory, "1.2.3")
	}
	for _, file := range files {
		contents, err := os.ReadFile(file)
		if err != nil || string(contents) != "fixture must stay unchanged\n" {
			t.Fatalf("unrelated file changed: %s, %q, %v", file, contents, err)
		}
	}
	install.assertClean()
}

func TestInstallRejectsDirectoryTargets(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%v", symlink), func(t *testing.T) {
			release := newRelease(t, "1.2.3", "linux", "amd64")
			install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
			directory := install.defaultDirectory()
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(directory, "codex-gateway")
			actualDirectory := target
			if symlink {
				actualDirectory = filepath.Join(install.home, "unrelated-directory")
			}
			if err := os.Mkdir(actualDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(actualDirectory, target); err != nil {
					t.Fatal(err)
				}
			}
			output, err := install.run()
			if err == nil || !strings.Contains(output, "target is a directory") {
				t.Fatalf("expected directory rejection: %v, %s", err, output)
			}
			entries, err := os.ReadDir(actualDirectory)
			if err != nil || len(entries) != 0 || release.total() != 0 {
				t.Fatalf("directory target changed or network used: %v, %v", entries, err)
			}
			install.assertClean()
		})
	}
}

func TestInstallDoesNotExtractArchivePaths(t *testing.T) {
	release := newRelease(t, "1.2.3", "linux", "amd64")
	install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
	escape := filepath.Join(install.root, "must-not-be-created")
	release.mu.Lock()
	release.setArchive(archiveWithEntries(t, map[string]string{
		"codex-gateway": versionExecutable("1.2.3"),
		"../escape":     "must not be extracted",
		escape:          "must not be extracted",
	}))
	release.mu.Unlock()
	output, err := install.run("--version", "1.2.3")
	if err != nil {
		t.Fatalf("install expected root member: %v\n%s", err, output)
	}
	assertVersion(t, install.defaultDirectory(), "1.2.3")
	for _, path := range []string{escape, filepath.Join(install.temp, "escape"), filepath.Join(install.defaultDirectory(), "escape")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("archive path was extracted: %s, %v", path, err)
		}
	}
	install.assertClean()
}

func TestInstallRejectsInvalidVersionsBeforeNetwork(t *testing.T) {
	for _, version := range []string{"", "v", "vv1.2.3", "01.2.3", "1.02.3", "1.2.03", "1.2.3-01", "1.2.3-alpha..1", "1.2.3+build..1", "../1.2.3", "1.2.3/path", "1.2.3?query", "1.2.3\nextra", `1.2.\063`, "1.2"} {
		t.Run(version, func(t *testing.T) {
			release := newRelease(t, "1.2.3", "linux", "amd64")
			install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
			output, err := install.run("--version", version)
			if err == nil || release.total() != 0 {
				t.Fatalf("invalid version accepted/network used: %v, %s", err, output)
			}
			if _, err := os.Stat(install.defaultDirectory()); !os.IsNotExist(err) {
				t.Fatalf("target directory was created: %v", err)
			}
			install.assertClean()
		})
	}
}

func TestInstallRejectsUnsupportedPlatformBeforeWrites(t *testing.T) {
	for _, test := range []struct{ system, machine string }{{"FreeBSD", "x86_64"}, {"Linux", "riscv64"}} {
		t.Run(test.system+" "+test.machine, func(t *testing.T) {
			release := newRelease(t, "1.2.3", "linux", "amd64")
			install := newInstallation(t, release, test.system, test.machine, "sha256sum")
			output, err := install.run()
			if err == nil || !strings.Contains(output, "Unsupported") || release.total() != 0 {
				t.Fatalf("unsupported platform accepted/network used: %v, %s", err, output)
			}
			if _, err := os.Stat(install.defaultDirectory()); !os.IsNotExist(err) {
				t.Fatalf("target directory was created: %v", err)
			}
			install.assertClean()
		})
	}
}

func TestInstallHelpNeedsNoUtilitiesOrNetwork(t *testing.T) {
	release := newRelease(t, "1.2.3", "linux", "amd64")
	install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
	install.values["PATH"] = filepath.Join(install.root, "no-tools")
	output, err := install.run("--help")
	if err != nil || !strings.Contains(output, "--install-dir") || release.total() != 0 {
		t.Fatalf("help failed: %v, %s", err, output)
	}
	entries, err := os.ReadDir(install.home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("help wrote files: %v, %v", entries, err)
	}
	install.assertClean()
}

func TestInstallRejectsUnsafeReleaseURLs(t *testing.T) {
	for _, url := range []string{"http://example.invalid/releases", "http://localhost.evil.invalid/releases", "http://127.0.0.1:0/releases", "http://user@localhost/releases", "file:///tmp/release", "https://example.invalid/releases?query", "https://example.invalid/releases#fragment", `https://example.invalid/\releases`} {
		t.Run(url, func(t *testing.T) {
			release := newRelease(t, "1.2.3", "linux", "amd64")
			install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
			install.values["CODEX_GATEWAY_RELEASE_BASE_URL"] = url
			output, err := install.run("--version", "1.2.3")
			if err == nil || release.total() != 0 {
				t.Fatalf("unsafe release URL accepted: %v, %s", err, output)
			}
			if _, err := os.Stat(install.defaultDirectory()); !os.IsNotExist(err) {
				t.Fatalf("target directory was created: %v", err)
			}
			install.assertClean()
		})
	}
}

func TestInstallRejectsUnexpectedLatestRedirect(t *testing.T) {
	for _, target := range []string{"https://example.invalid/tag/v1.2.3", "/tag/v01.2.3", "/tag/v1.2.3?query", "/somewhere-else"} {
		t.Run(target, func(t *testing.T) {
			release := newRelease(t, "1.2.3", "linux", "amd64")
			release.mu.Lock()
			release.latestTarget = target
			release.mu.Unlock()
			install := newInstallation(t, release, "Linux", "x86_64", "sha256sum")
			output, err := install.run()
			if err == nil || release.total() != 1 || release.count("HEAD", "/latest") != 1 {
				t.Fatalf("unexpected latest redirect followed/accepted: %v, %s", err, output)
			}
			if _, err := os.Stat(install.defaultDirectory()); !os.IsNotExist(err) {
				t.Fatalf("target directory was created: %v", err)
			}
			install.assertClean()
		})
	}
}
