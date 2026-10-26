// Command labcheck checks your repository before you submit Part 1:
//
//	go run ./cmd/labcheck          # framework files intact? builds? public tests and score
//	go run ./cmd/labcheck -quick   # skip the tests
//
// Grading uses the original framework files and only your chain/ files (the five ★ files
// and any new non-test .go files you added there), with the official test files. So a
// change to a framework file is lost at grading time, and labcheck warns about it.
package main

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed weights.json
var weightsJSON []byte

// studentFiles are the files you write for Part 1.
var studentFiles = []string{"chain/pow.go", "chain/consensus.go", "chain/process.go", "chain/txpool.go",
	"chain/blockchain.go", "chain/miner.go", "chain/sync.go"}

func main() {
	quick := flag.Bool("quick", false, "do not run the tests")
	flag.Parse()
	root, err := moduleRoot()
	if err != nil {
		fail(err)
	}
	if err := os.Chdir(root); err != nil {
		fail(err)
	}
	ok := true
	fmt.Println("== framework files (lab-manifest.txt)")
	manifest, err := readManifest("lab-manifest.txt")
	if err != nil {
		fail(fmt.Errorf("%v (run labcheck from inside the lab repository)", err))
	}
	var changed, missing []string
	for _, e := range manifest {
		sum, err := fileSum(e.path)
		switch {
		case err != nil:
			missing = append(missing, e.path)
		case sum != e.sum:
			changed = append(changed, e.path)
		}
	}
	if len(changed)+len(missing) == 0 {
		fmt.Printf("   all %d framework files are unchanged\n", len(manifest))
	}
	for _, p := range changed {
		fmt.Printf("   WARNING modified: %s (grading uses the original: your change is lost)\n", p)
		ok = false
	}
	for _, p := range missing {
		fmt.Printf("   WARNING missing:  %s\n", p)
		ok = false
	}
	inManifest := map[string]bool{}
	for _, e := range manifest {
		inManifest[e.path] = true
	}
	for _, f := range studentFiles {
		inManifest[f] = true
		if _, err := os.Stat(f); err != nil {
			fmt.Printf("   WARNING your file %s is missing\n", f)
			ok = false
		}
	}
	extra, _ := filepath.Glob("chain/*.go")
	for _, f := range extra {
		f = filepath.ToSlash(f)
		switch {
		case inManifest[f]:
		case strings.HasSuffix(f, "_test.go"):
			fmt.Printf("   note: %s is your own test file; grading uses only the official tests\n", f)
		default:
			fmt.Printf("   note: %s is your own file in chain/; it is kept for grading\n", f)
		}
	}

	fmt.Println("== go build ./... and go vet ./chain ./tests")
	if out, err := run("go", "build", "./..."); err != nil {
		fmt.Printf("   FAILED:\n%s\n", indent(out))
		os.Exit(1)
	}
	if out, err := run("go", "vet", "./chain/", "./tests/"); err != nil {
		fmt.Printf("   go vet found problems:\n%s\n", indent(out))
		ok = false
	} else {
		fmt.Println("   ok")
	}
	if *quick {
		finish(ok)
		return
	}

	fmt.Println("== public tests (go test ./chain ./tests), this takes a minute or two")
	var weights map[string]float64
	raw := map[string]any{}
	json.Unmarshal(weightsJSON, &raw) //nolint:errcheck
	weights = map[string]float64{}
	for k, v := range raw {
		if f, isNum := v.(float64); isNum {
			weights[k] = f
		}
	}
	results, panicked := runTests()
	names := make([]string, 0, len(weights))
	for n := range weights {
		names = append(names, n)
	}
	sort.Strings(names)
	var got, total float64
	for _, n := range names {
		total += weights[n]
		mark := "FAIL"
		if results[n] == "pass" {
			mark = "ok  "
			got += weights[n]
		} else if results[n] == "" {
			mark = "----" // did not run (a panic or a build failure stopped it)
		}
		fmt.Printf("   %s %-40s %5.2f\n", mark, n, weights[n])
	}
	fmt.Printf("   public tests: %.2f / %g points (the hidden tests are worth 35 more)\n", got, total)
	if got < total {
		fmt.Println("   some public tests fail: run them with -v for details, e.g. go test ./chain -run TestChain -v")
	}
	if panicked {
		fmt.Println("   NOTE: a test panicked, which stops the tests after it in the same package: fix that first.")
	}
	for name, res := range results {
		if strings.HasPrefix(name, "TestFramework") && res != "pass" {
			fmt.Printf("   WARNING framework test %s failed: the framework is broken, did you change a framework file?\n", name)
			ok = false
		}
	}
	finish(ok)
}

func finish(ok bool) {
	if ok {
		fmt.Println("== framework files and build: ok")
	} else {
		fmt.Println("== see the warnings above")
	}
}

type entry struct{ sum, path string }

func readManifest(path string) ([]entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 {
			out = append(out, entry{parts[0], parts[1]})
		}
	}
	return out, sc.Err()
}

// fileSum hashes a file with line endings normalised (Windows checkouts may use CRLF).
func fileSum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// runTests runs the public tests and returns test name -> "pass"/"fail".
func runTests() (map[string]string, bool) {
	cmd := exec.Command("go", "test", "-count=1", "-timeout=15m", "-json", "./chain/", "./tests/")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fail(err)
	}
	results := map[string]string{}
	panicked := false
	dec := json.NewDecoder(stdout)
	for {
		var ev struct {
			Action, Test, Output string
		}
		if err := dec.Decode(&ev); err == io.EOF {
			break
		} else if err != nil {
			break
		}
		if strings.Contains(ev.Output, "panic: ") {
			panicked = true
		}
		if ev.Test != "" && (ev.Action == "pass" || ev.Action == "fail") && !strings.Contains(ev.Test, "/") {
			results[ev.Test] = ev.Action
		}
	}
	cmd.Wait() //nolint:errcheck // failures are in the results
	return results, panicked
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func indent(s string) string {
	return "   " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n   ")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "labcheck:", err)
	os.Exit(1)
}
