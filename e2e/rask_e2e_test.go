package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestColdThenWarm(t *testing.T) {
	module := scaffoldModule(t, demoPackages(true)...)
	cacheDir := t.TempDir()
	cold := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertCode(t, cold, 0)
	assertSuites(t, cold.stdout, "Passed", "bystander", "dep", "dependent", "slow")
	warm := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertCode(t, warm, 0)
	if warm.duration >= time.Second {
		t.Fatalf("warm run took %s, want <1s\n%s", warm.duration, warm.stdout)
	}
	assertSuites(t, warm.stdout, "Cached", "bystander", "dep", "dependent", "slow")
	assertContains(t, warm.stdout, "4 cached")
}

func TestEditInvalidation(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	editDepSource(t, module)
	result := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "dep", "dependent")
	assertSuites(t, result.stdout, "Cached", "bystander")
	assertNotContains(t, result.stdout, "Passed bystander")
}

func TestFailFast(t *testing.T) {
	packages := append(demoPackages(false), packageSpec{name: "failer", test: testFail})
	packages = append(packages, packageSpec{name: "slow", test: testPass, sleep: 2 * time.Second})
	module := scaffoldModule(t, packages...)
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, result, 1)
	combined := result.stdout + result.stderr
	assertContains(t, combined, "error: test failed in failer")
	assertContains(t, combined, "skipped")
	waitNoGoTestProcesses(t, module.path)
}

func TestBrokenPackage(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "broken", test: testPass})
	writeFile(t, module.dir, filepath.Join("broken", "broken.go"), "package broken\n\nfunc Broken( {\n")
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, result, 1)
	assertContains(t, result.stderr, "broken")
	assertNotContains(t, result.stdout, "Passed")
	assertNotContains(t, result.stdout, "Finished")
}

func TestNoCacheFlag(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	assertSuites(t, runRask(t, module.dir, cacheEnv(cacheDir), "./...").stdout, "Cached", "bystander", "dep", "dependent")
	result := runRask(t, module.dir, cacheEnv(cacheDir), "--no-cache", "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "bystander", "dep", "dependent")
	assertNotContains(t, result.stdout, "Cached ")
	third := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertSuites(t, third.stdout, "Cached", "bystander", "dep", "dependent")
}

func TestConcurrencyFlag(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "dep", test: testPass})
	success := runRask(t, module.dir, cacheEnv(t.TempDir()), "--concurrency", "1", "./...")
	assertCode(t, success, 0)
	assertSuites(t, success.stdout, "Passed", "dep")
	failure := runRask(t, module.dir, cacheEnv(t.TempDir()), "--concurrency", "-1")
	assertCode(t, failure, 1)
	assertContains(t, failure.stderr, "--concurrency must be positive")
}

func TestCleanCommand(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	assertSuites(t, runRask(t, module.dir, cacheEnv(cacheDir), "./...").stdout, "Cached", "dep")
	clean := runRask(t, module.dir, cacheEnv(cacheDir), "clean")
	assertCode(t, clean, 0)
	assertContains(t, clean.stdout, "Removed "+module.path)
	assertSuites(t, runRask(t, module.dir, cacheEnv(cacheDir), "./...").stdout, "Passed", "dep")
	assertForeignCacheRefused(t, module)
}

func TestTiered(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv, stop := startFakeS3(t)
	assertTieredScenario(t, module, remoteEnv, func(env map[string]string) {
		env["RASK_REMOTE_CACHE_ACCESS_KEY_ID"] = "wrong"
	}, stop)
}

func TestTieredMinIO(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv := startMinio(t)
	assertTieredScenario(t, module, remoteEnv, func(env map[string]string) {
		env["RASK_REMOTE_CACHE_SECRET_ACCESS_KEY"] = "wrong"
	}, func() {})
}

func TestTieredExternalS3(t *testing.T) {
	bucket := os.Getenv("RASK_E2E_S3_BUCKET")
	if bucket == "" {
		t.Skip("RASK_E2E_S3_BUCKET is unset; skipping real S3-compatible e2e")
	}
	required := []string{"RASK_E2E_S3_ACCESS_KEY_ID", "RASK_E2E_S3_SECRET_ACCESS_KEY"}
	for _, name := range required {
		if os.Getenv(name) == "" {
			t.Skipf("%s is unset; skipping real S3-compatible e2e", name)
		}
	}
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv := externalS3Env(bucket)
	assertTieredScenario(t, module, remoteEnv, func(env map[string]string) {
		env["RASK_REMOTE_CACHE_SECRET_ACCESS_KEY"] = "wrong"
	}, func() {})
}

func externalS3Env(bucket string) map[string]string {
	env := map[string]string{
		"RASK_REMOTE_CACHE_BUCKET":            bucket,
		"RASK_REMOTE_CACHE_ACCESS_KEY_ID":     os.Getenv("RASK_E2E_S3_ACCESS_KEY_ID"),
		"RASK_REMOTE_CACHE_SECRET_ACCESS_KEY": os.Getenv("RASK_E2E_S3_SECRET_ACCESS_KEY"),
	}
	copyE2ES3Env(env, "PREFIX", "ENDPOINT", "REGION")
	return env
}

func assertTieredScenario(
	t *testing.T,
	module moduleFixture,
	remoteEnv map[string]string,
	breakCredentials func(map[string]string),
	stop func(),
) {
	t.Helper()
	machineA := envWithCache(t.TempDir(), remoteEnv)
	assertCode(t, runRask(t, module.dir, machineA, "./..."), 0)
	machineB := envWithCache(t.TempDir(), remoteEnv)
	machineBRemote := runRask(t, module.dir, machineB, "./...")
	assertCode(t, machineBRemote, 0)
	assertSuites(t, machineBRemote.stdout, "Cached", "bystander", "dep", "dependent")
	assertContains(t, machineBRemote.stdout, "3 cached (3 remote) | remote: 3 reads, 0 writes")
	badEnv := copyMap(remoteEnv)
	breakCredentials(badEnv)
	result := runRask(t, module.dir, envWithCache(t.TempDir(), badEnv), "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "bystander", "dep", "dependent")
	assertNotContains(t, result.stdout, "Cached ")
	stop()
	assertSuites(t, runRask(t, module.dir, machineB, "./...").stdout, "Cached", "bystander", "dep", "dependent")
}

func TestStallWatchdog(t *testing.T) {
	t.Skip("rask's subprocess stall watchdog is intentionally fixed at 5m; e2e keeps this gap documented instead of adding a test-only flag or a slow suite.")
}

func TestVerbose(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "dep", test: testPass})
	quiet := runRask(t, module.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, quiet, 0)
	assertNotContains(t, quiet.stdout, "test TestDep ... ok")
	verbose := runRask(t, module.dir, cacheEnv(t.TempDir()), "-v", "./...")
	assertCode(t, verbose, 0)
	assertContains(t, verbose.stdout, "test TestDep ... ok")
	failing := scaffoldModule(t, packageSpec{name: "failer", test: testFail})
	failed := runRask(t, failing.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, failed, 1)
	assertContains(t, failed.stdout, "test TestFailer ... FAILED")
}

func assertForeignCacheRefused(t *testing.T, module moduleFixture) {
	t.Helper()
	foreign := t.TempDir()
	tag := filepath.Join(foreign, "CACHEDIR.TAG")
	sentinel := filepath.Join(foreign, "sentinel")
	if err := os.WriteFile(tag, []byte("Signature: foreign\n"), 0o644); err != nil {
		t.Fatalf("write foreign tag: %v", err)
	}
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	result := runRask(t, module.dir, cacheEnv(foreign), "clean")
	assertCode(t, result, 1)
	assertContains(t, result.stderr, "non-rask CACHEDIR.TAG")
	assertFileContains(t, tag, "Signature: foreign\n")
	assertFileContains(t, sentinel, "keep")
}

func envWithCache(cacheDir string, extra map[string]string) map[string]string {
	env := cacheEnv(cacheDir)
	for key, value := range extra {
		env[key] = value
	}
	return env
}

func copyMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func copyE2ES3Env(target map[string]string, names ...string) {
	for _, name := range names {
		key := "RASK_E2E_S3_" + name
		if value := os.Getenv(key); value != "" {
			target["RASK_REMOTE_CACHE_"+name] = value
		}
	}
}

func waitNoGoTestProcesses(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processes := goTestProcesses(t, marker); processes == "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("orphaned go test process remains:\n%s", goTestProcesses(t, marker))
}

func goTestProcesses(t *testing.T, marker string) string {
	t.Helper()
	output, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("process listing unavailable in this sandbox: %v", err)
		}
		t.Fatalf("list processes: %v", err)
	}
	var matches []string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "go test") && strings.Contains(line, marker) {
			matches = append(matches, strings.TrimSpace(line))
		}
	}
	return strings.Join(matches, "\n")
}

func assertSuites(t *testing.T, output string, verb string, names ...string) {
	t.Helper()
	for _, name := range names {
		assertContains(t, output, fmt.Sprintf("%s %s", verb, name))
	}
}

func assertCode(t *testing.T, result raskResult, want int) {
	t.Helper()
	if result.code != want {
		t.Fatalf("exit code=%d, want %d\nstdout:\n%s\nstderr:\n%s", result.code, want, result.stdout, result.stderr)
	}
}

func assertContains(t *testing.T, text string, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("missing %q in:\n%s", want, text)
	}
}

func assertNotContains(t *testing.T, text string, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		t.Fatalf("unexpected %q in:\n%s", unwanted, text)
	}
}

func assertFileContains(t *testing.T, path string, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}
