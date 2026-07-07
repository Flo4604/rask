package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	raskBinary string
	goCacheDir = "/tmp/rask-gocache"
	goTempDir  = "/tmp/rask-gotmp"
)

type raskResult struct {
	code     int
	stdout   string
	stderr   string
	duration time.Duration
}

type moduleFixture struct {
	dir  string
	path string
}

type packageSpec struct {
	name    string
	imports []string
	test    testKind
	sleep   time.Duration
}

type testKind string

const (
	testPass testKind = "pass"
	testFail testKind = "fail"
	testSkip testKind = "skip"
	testNone testKind = "none"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rask-e2e-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create e2e bin dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(goCacheDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create GOCACHE: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(goTempDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create GOTMPDIR: %v\n", err)
		os.Exit(1)
	}
	raskBinary = filepath.Join(dir, "rask")
	command := exec.Command("go", "build", "-o", raskBinary, "..")
	command.Env = baseEnv(nil)
	output, err := command.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "building rask e2e binary failed: %v\n%s", err, output)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func runRask(t *testing.T, dir string, env map[string]string, args ...string) raskResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, raskBinary, args...)
	command.Dir = dir
	command.Env = baseEnv(env)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	startedAt := time.Now()
	err := command.Run()
	result := raskResult{stdout: stdout.String(), stderr: stderr.String(), duration: time.Since(startedAt)}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("rask %v timed out\nstdout:\n%s\nstderr:\n%s", args, result.stdout, result.stderr)
	}
	if err == nil {
		return result
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("rask %v failed to start: %v", args, err)
	}
	result.code = exit.ExitCode()
	return result
}

// baseEnv builds a hermetic child environment: inherited RASK_* variables are
// stripped and RASK_CONFIG points at a file that does not exist, so spawned
// rask processes can never read the developer's real ~/.rask.json or ambient
// remote-cache credentials. Remote-tier scenarios opt back in through extra.
func baseEnv(extra map[string]string) []string {
	values := map[string]string{
		"GOCACHE":     goCacheDir,
		"GOTMPDIR":    goTempDir,
		"GOWORK":      "off",
		"NO_COLOR":    "1",
		"RASK_CONFIG": filepath.Join(goTempDir, "rask-e2e-no-config.json"),
	}
	for key, value := range extra {
		values[key] = value
	}
	out := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "RASK_") {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func cacheEnv(dir string) map[string]string {
	return map[string]string{"RASK_CACHE_DIR": dir}
}

func scaffoldModule(t *testing.T, packages ...packageSpec) moduleFixture {
	t.Helper()
	dir := t.TempDir()
	path := "example.test/" + sanitizeName(t.Name())
	writeFile(t, dir, "go.mod", "module "+path+"\n\ngo 1.25\n")
	for _, spec := range packages {
		writePackage(t, dir, path, spec)
	}
	return moduleFixture{dir: dir, path: path}
}

func demoPackages(includeSlow bool) []packageSpec {
	packages := []packageSpec{
		{name: "dep", test: testPass},
		{name: "dependent", imports: []string{"dep"}, test: testPass},
		{name: "bystander", test: testPass},
	}
	if includeSlow {
		packages = append(packages, packageSpec{name: "slow", test: testPass, sleep: 2 * time.Second})
	}
	return packages
}

func writePackage(t *testing.T, dir string, modulePath string, spec packageSpec) {
	t.Helper()
	pkgDir := filepath.Join(dir, spec.name)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("create package %s: %v", spec.name, err)
	}
	writeFile(t, dir, filepath.Join(spec.name, spec.name+".go"), sourceFor(modulePath, spec))
	if spec.test != testNone {
		writeFile(t, dir, filepath.Join(spec.name, spec.name+"_test.go"), testFor(spec))
	}
}

func sourceFor(modulePath string, spec packageSpec) string {
	var out strings.Builder
	fmt.Fprintf(&out, "package %s\n\n", spec.name)
	if len(spec.imports) > 0 {
		out.WriteString("import (\n")
		for _, imported := range spec.imports {
			fmt.Fprintf(&out, "\t%q\n", modulePath+"/"+imported)
		}
		out.WriteString(")\n\n")
	}
	out.WriteString("func Value() int {\n\ttotal := 1\n")
	for _, imported := range spec.imports {
		fmt.Fprintf(&out, "\ttotal += %s.Value()\n", imported)
	}
	out.WriteString("\treturn total\n}\n")
	return out.String()
}

func testFor(spec packageSpec) string {
	name := "Test" + strings.ToUpper(spec.name[:1]) + spec.name[1:]
	var out strings.Builder
	fmt.Fprintf(&out, "package %s\n\nimport (\n\t\"testing\"\n", spec.name)
	if spec.sleep > 0 {
		out.WriteString("\t\"time\"\n")
	}
	out.WriteString(")\n\n")
	fmt.Fprintf(&out, "func %s(t *testing.T) {\n", name)
	if spec.sleep > 0 {
		fmt.Fprintf(&out, "\ttime.Sleep(%d * time.Millisecond)\n", spec.sleep.Milliseconds())
	}
	writeTestBody(&out, spec)
	out.WriteString("}\n")
	return out.String()
}

func writeTestBody(out *strings.Builder, spec packageSpec) {
	switch spec.test {
	case testPass:
		out.WriteString("\tif Value() <= 0 {\n\t\tt.Fatal(\"bad value\")\n\t}\n")
	case testFail:
		fmt.Fprintf(out, "\tt.Fatalf(\"intentional failure in %s\")\n", spec.name)
	case testSkip:
		out.WriteString("\tt.Skip(\"intentional skip\")\n")
	default:
		out.WriteString("\tt.Fatal(\"unknown test kind\")\n")
	}
}

func writeFile(t *testing.T, root string, name string, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func editDepSource(t *testing.T, module moduleFixture) {
	t.Helper()
	body := "package dep\n\nfunc Value() int {\n\treturn 2\n}\n"
	writeFile(t, module.dir, filepath.Join("dep", "dep.go"), body)
}

func sanitizeName(name string) string {
	name = strings.ToLower(name)
	replacer := strings.NewReplacer("/", "-", "_", "-", " ", "-")
	return strings.Trim(replacer.Replace(name), "-")
}

func startFakeS3(t *testing.T) (map[string]string, func()) {
	t.Helper()
	server := newLoopbackHTTPServer(t, fakeS3Handler("fake-access"))
	env := s3Env("rask-e2e", "prefix", server.URL, "fake-access", "fake-secret")
	stop := server.Close
	t.Cleanup(stop)
	return env, stop
}

// startDelayedFakeS3 isolates interrupt-latency tests from normal fake S3 speed.
func startDelayedFakeS3(t *testing.T, delay time.Duration) (map[string]string, func()) {
	t.Helper()
	server := newLoopbackHTTPServer(t, delayedHandler(fakeS3Handler("fake-access"), delay))
	env := s3Env("rask-e2e", "prefix", server.URL, "fake-access", "fake-secret")
	stop := server.Close
	t.Cleanup(stop)
	return env, stop
}

// newLoopbackHTTPServer avoids httptest's default IPv6 listener in restricted sandboxes.
func newLoopbackHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := &httptest.Server{
		Listener: loopbackListener(t),
		Config:   &http.Server{Handler: handler},
	}
	server.Start()
	return server
}

// delayedHandler adds deterministic remote latency without changing S3 semantics.
func delayedHandler(next http.Handler, delay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			next.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	})
}

func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("loopback bind unavailable in this sandbox: %v", err)
		}
		t.Fatalf("allocate loopback port: %v", err)
	}
	return listener
}

func startMinio(t *testing.T) map[string]string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if output, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v\n%s", err, output)
	}
	id := dockerRunMinio(t)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port := dockerPort(t, id)
	waitMinio(t, port)
	dockerExec(t, id, "mkdir", "-p", "/data/rask-e2e")
	return s3Env("rask-e2e", "", "http://127.0.0.1:"+port, "rask-access", "rask-secret")
}

func fakeS3Handler(accessKey string) http.Handler {
	var mu sync.Mutex
	objects := map[string][]byte{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential="+accessKey+"/") {
			http.Error(w, "bad credentials", http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			serveFakeS3List(w, r, key, &mu, objects)
			return
		}
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			body, found := objects[key]
			mu.Unlock()
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
				return
			}
			_, _ = w.Write(append([]byte(nil), body...))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[key] = append([]byte(nil), body...)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			mu.Lock()
			delete(objects, key)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func serveFakeS3List(
	w http.ResponseWriter,
	r *http.Request,
	bucket string,
	mu *sync.Mutex,
	objects map[string][]byte,
) {
	prefix := bucket + "/" + r.URL.Query().Get("prefix")
	mu.Lock()
	keys := make([]string, 0, len(objects))
	for key := range objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, strings.TrimPrefix(key, bucket+"/"))
		}
	}
	mu.Unlock()
	sort.Strings(keys)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated>"))
	for _, key := range keys {
		_, _ = fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", key)
	}
	_, _ = w.Write([]byte("</ListBucketResult>"))
}

// dockerRunMinio reads stdout only: `docker run -d` writes exactly the
// container ID there, while image-pull progress goes to stderr. Mixing the
// streams turned first-pull CI runs into a garbage "container ID" and broke
// every docker command that received it.
func dockerRunMinio(t *testing.T) string {
	t.Helper()
	image := "minio/minio:RELEASE.2024-01-16T16-07-38Z"
	command := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1:0:9000",
		"-e", "MINIO_ROOT_USER=rask-access", "-e", "MINIO_ROOT_PASSWORD=rask-secret",
		image, "server", "/data")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("docker run minio: %v\n%s", err, stderr.String())
	}
	id := strings.TrimSpace(string(output))
	if id == "" || strings.ContainsAny(id, " \n") {
		t.Fatalf("unexpected docker run output %q\nstderr:\n%s", output, stderr.String())
	}
	return id
}

func dockerPort(t *testing.T, id string) string {
	t.Helper()
	output, err := exec.Command("docker", "port", id, "9000").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port: %v\n%s", err, output)
	}
	address := strings.TrimSpace(string(output))
	if index := strings.LastIndex(address, ":"); index >= 0 {
		return address[index+1:]
	}
	t.Fatalf("unexpected docker port output %q", address)
	return ""
}

func waitMinio(t *testing.T, port string) {
	t.Helper()
	url := "http://127.0.0.1:" + port + "/minio/health/live"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil && response.StatusCode == http.StatusOK {
			_ = response.Body.Close()
			return
		}
		if response != nil {
			_ = response.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("minio did not become healthy at %s", url)
}

func dockerExec(t *testing.T, id string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"exec", id}, args...)
	if output, err := exec.Command("docker", commandArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker exec %v: %v\n%s", args, err, output)
	}
}

func s3Env(bucket string, prefix string, endpoint string, accessKey string, secretKey string) map[string]string {
	return map[string]string{
		"RASK_REMOTE_CACHE_BUCKET":            bucket,
		"RASK_REMOTE_CACHE_PREFIX":            prefix,
		"RASK_REMOTE_CACHE_ENDPOINT":          endpoint,
		"RASK_REMOTE_CACHE_ACCESS_KEY_ID":     accessKey,
		"RASK_REMOTE_CACHE_SECRET_ACCESS_KEY": secretKey,
		"RASK_REMOTE_CACHE_REGION":            "us-east-1",
	}
}
