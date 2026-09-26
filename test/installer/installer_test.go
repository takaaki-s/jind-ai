package installer_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallerResolvesLatestVerifiesAndInstalls(t *testing.T) {
	fixture := newInstallerFixture(t, "Linux", "x86_64", "9.8.7")
	result := fixture.run(t)

	if !strings.Contains(result, "Installed jin 9.8.7") {
		t.Fatalf("output = %q", result)
	}
	installed, err := os.ReadFile(filepath.Join(fixture.installDir, "jin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != fixture.binaryContent {
		t.Fatalf("installed binary = %q", installed)
	}
	info, err := os.Stat(filepath.Join(fixture.installDir, "jin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %o, want 755", info.Mode().Perm())
	}
	log := fixture.curlLog(t)
	for _, want := range []string{
		"/releases/latest",
		"/releases/download/v9.8.7/jind-ai_9.8.7_linux_amd64.tar.gz",
		"/releases/download/v9.8.7/checksums.txt",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("curl log does not contain %q:\n%s", want, log)
		}
	}
}

func TestInstallerAcceptsPinnedVersionAndDarwinArm64(t *testing.T) {
	fixture := newInstallerFixture(t, "Darwin", "arm64", "1.2.3")
	fixture.args = []string{"--version", "v1.2.3", "--bin-dir", fixture.installDir}
	fixture.run(t)

	log := fixture.curlLog(t)
	if strings.Contains(log, "/releases/latest") {
		t.Fatalf("pinned install resolved latest:\n%s", log)
	}
	if !strings.Contains(log, "/releases/download/v1.2.3/jind-ai_1.2.3_darwin_arm64.tar.gz") {
		t.Fatalf("curl log = %s", log)
	}
}

func TestInstallerDefaultsToHomeLocalBin(t *testing.T) {
	fixture := newInstallerFixture(t, "Linux", "amd64", "3.4.5")
	fixture.args = nil
	fixture.installDir = filepath.Join(fixture.homeDir, ".local", "bin")
	fixture.run(t)

	if _, err := os.Stat(filepath.Join(fixture.installDir, "jin")); err != nil {
		t.Fatalf("default installation: %v", err)
	}
}

func TestInstallerRefusesChecksumMismatchWithoutReplacingBinary(t *testing.T) {
	fixture := newInstallerFixture(t, "Linux", "aarch64", "2.0.0")
	fixture.args = []string{"--version", "2.0.0", "--bin-dir", fixture.installDir}
	if err := os.MkdirAll(fixture.installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(fixture.installDir, "jin")
	if err := os.WriteFile(existing, []byte("existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.checksums, []byte(strings.Repeat("0", 64)+"  jind-ai_2.0.0_linux_arm64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := fixture.command(t)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("installer succeeded with a bad checksum:\n%s", output)
	}
	if !strings.Contains(string(output), "checksum verification failed") {
		t.Fatalf("output = %q", output)
	}
	got, readErr := os.ReadFile(existing)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "existing" {
		t.Fatalf("existing binary was replaced: %q", got)
	}
}

type installerFixture struct {
	script        string
	fakeBin       string
	archive       string
	checksums     string
	curlLogPath   string
	installDir    string
	binaryContent string
	version       string
	osName        string
	arch          string
	homeDir       string
	args          []string
}

func newInstallerFixture(t *testing.T, osName, arch, version string) *installerFixture {
	t.Helper()
	root := t.TempDir()
	f := &installerFixture{
		fakeBin: filepath.Join(root, "fake-bin"),
		archive: filepath.Join(root, "release.tar.gz"), checksums: filepath.Join(root, "checksums.txt"),
		curlLogPath: filepath.Join(root, "curl.log"), installDir: filepath.Join(root, "install"),
		binaryContent: "fixture jin\n", version: version, osName: osName, arch: arch,
		homeDir: filepath.Join(root, "home"), args: []string{"--bin-dir", filepath.Join(root, "install")},
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	f.script = filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "install.sh"))

	if err := os.MkdirAll(f.fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeArchive(t, f.archive, f.binaryContent)
	archiveBytes, err := os.ReadFile(f.archive)
	if err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("jind-ai_%s_%s_%s.tar.gz", version, strings.ToLower(osName), normalizedArch(arch))
	checksum := fmt.Sprintf("%x  %s\n", sha256.Sum256(archiveBytes), archiveName)
	if err := os.WriteFile(f.checksums, []byte(checksum), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(f.fakeBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' "$FAKE_UNAME_S" ;;
  -m) printf '%s\n' "$FAKE_UNAME_M" ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(f.fakeBin, "curl"), `#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
case "$url" in
  */releases/latest) printf 'https://github.com/takaaki-s/jind-ai/releases/tag/v%s' "$FAKE_VERSION" ;;
  */checksums.txt) cp "$FAKE_CHECKSUMS" "$out" ;;
  *.tar.gz) cp "$FAKE_ARCHIVE" "$out" ;;
  *) exit 22 ;;
esac
`)
	return f
}

func (f *installerFixture) run(t *testing.T) string {
	t.Helper()
	cmd := f.command(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, output)
	}
	return string(output)
}

func (f *installerFixture) command(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", append([]string{f.script}, f.args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+f.fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+f.homeDir,
		"FAKE_UNAME_S="+f.osName,
		"FAKE_UNAME_M="+f.arch,
		"FAKE_VERSION="+f.version,
		"FAKE_ARCHIVE="+f.archive,
		"FAKE_CHECKSUMS="+f.checksums,
		"FAKE_CURL_LOG="+f.curlLogPath,
	)
	return cmd
}

func (f *installerFixture) curlLog(t *testing.T) string {
	t.Helper()
	log, err := os.ReadFile(f.curlLogPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(log)
}

func writeArchive(t *testing.T, path, content string) {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "jin", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func normalizedArch(arch string) string {
	switch arch {
	case "x86_64", "amd64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return arch
	}
}
