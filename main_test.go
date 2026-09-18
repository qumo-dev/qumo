package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test cases that exercise main() by re-executing the test binary in a child
// process. The child path is selected with -test.run and an env var toggles
// child behavior. This avoids calling os.Exit() in the test process.

func TestRun_Unit(t *testing.T) {
	origRelay := runRelay
	origRTMP := runRTMP
	origPlayground := runPlayground
	origDoctor := runDoctor
	origLoadgen := runLoadgen
	origUpdate := runUpdate
	defer func() {
		runRelay = origRelay
		runRTMP = origRTMP
		runPlayground = origPlayground
		runDoctor = origDoctor
		runLoadgen = origLoadgen
		runUpdate = origUpdate
	}()

	tests := map[string]struct {
		args               []string
		stubRelay          func([]string) error
		stubRTMP           func([]string) error
		stubPlayground     func([]string) error
		stubDoctor         func([]string) error
		stubLoadgen        func([]string) error
		stubUpdate         func([]string) error
		wantCode           int
		wantStderrContains []string
	}{
		"no args": {
			args:               []string{},
			wantCode:           1,
			wantStderrContains: []string{"Usage: qumo"},
		},
		"unknown command": {
			args:               []string{"badcmd"},
			wantCode:           1,
			wantStderrContains: []string{"unknown command"},
		},
		"relay success": {
			args:      []string{"relay"},
			stubRelay: func(_ []string) error { return nil },
			wantCode:  0,
		},
		"relay error": {
			args:               []string{"relay"},
			stubRelay:          func(_ []string) error { return fmt.Errorf("boom") },
			wantCode:           1,
			wantStderrContains: []string{"error: boom"},
		},
		"relay passes args": {
			args: []string{"relay", "extra"},
			stubRelay: func(a []string) error {
				assert.Equal(t, []string{"extra"}, a)
				return nil
			},
			wantCode: 0,
		},
		"rtmp success": {
			args:     []string{"rtmp"},
			stubRTMP: func(_ []string) error { return nil },
			wantCode: 0,
		},
		"rtmp error": {
			args:               []string{"rtmp"},
			stubRTMP:           func(_ []string) error { return fmt.Errorf("rtmp-fail") },
			wantCode:           1,
			wantStderrContains: []string{"error: rtmp-fail"},
		},
		"playground success": {
			args:           []string{"playground"},
			stubPlayground: func(_ []string) error { return nil },
			wantCode:       0,
		},
		"playground error": {
			args:               []string{"playground"},
			stubPlayground:     func(_ []string) error { return fmt.Errorf("pg-fail") },
			wantCode:           1,
			wantStderrContains: []string{"error: pg-fail"},
		},
		"playground passes args": {
			args: []string{"playground", "extra"},
			stubPlayground: func(a []string) error {
				assert.Equal(t, []string{"extra"}, a)
				return nil
			},
			wantCode: 0,
		},
		"doctor success": {
			args:       []string{"doctor"},
			stubDoctor: func(_ []string) error { return nil },
			wantCode:   0,
		},
		"doctor error": {
			args:               []string{"doctor"},
			stubDoctor:         func(_ []string) error { return fmt.Errorf("doc-fail") },
			wantCode:           1,
			wantStderrContains: []string{"error: doc-fail"},
		},
		"loadgen success": {
			args:        []string{"loadgen", "subscribe"},
			stubLoadgen: func(_ []string) error { return nil },
			wantCode:    0,
		},
		"loadgen error": {
			args:               []string{"loadgen"},
			stubLoadgen:        func(_ []string) error { return fmt.Errorf("lg-fail") },
			wantCode:           1,
			wantStderrContains: []string{"error: lg-fail"},
		},
		"loadgen passes args": {
			args: []string{"loadgen", "subscribe", "--sessions", "10"},
			stubLoadgen: func(a []string) error {
				assert.Equal(t, []string{"subscribe", "--sessions", "10"}, a)
				return nil
			},
			wantCode: 0,
		},
		"update success": {
			args:       []string{"update"},
			stubUpdate: func(_ []string) error { return nil },
			wantCode:   0,
		},
		"update error": {
			args:               []string{"update"},
			stubUpdate:         func(_ []string) error { return fmt.Errorf("update-fail") },
			wantCode:           1,
			wantStderrContains: []string{"error: update-fail"},
		},
		"update passes args": {
			args: []string{"update", "--check"},
			stubUpdate: func(a []string) error {
				assert.Equal(t, []string{"--check"}, a)
				return nil
			},
			wantCode: 0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.stubRelay != nil {
				runRelay = tt.stubRelay
			} else {
				runRelay = func([]string) error { return nil }
			}
			if tt.stubRTMP != nil {
				runRTMP = tt.stubRTMP
			} else {
				runRTMP = func([]string) error { return nil }
			}
			if tt.stubPlayground != nil {
				runPlayground = tt.stubPlayground
			} else {
				runPlayground = func([]string) error { return nil }
			}
			if tt.stubDoctor != nil {
				runDoctor = tt.stubDoctor
			} else {
				runDoctor = func([]string) error { return nil }
			}
			if tt.stubLoadgen != nil {
				runLoadgen = tt.stubLoadgen
			} else {
				runLoadgen = func([]string) error { return nil }
			}
			if tt.stubUpdate != nil {
				runUpdate = tt.stubUpdate
			} else {
				runUpdate = func([]string) error { return nil }
			}

			// capture stderr
			saved := os.Stderr
			r, w, err := os.Pipe()
			require.NoError(t, err)
			os.Stderr = w

			code := run(tt.args)

			_ = w.Close()
			var buf bytes.Buffer
			_, err = buf.ReadFrom(r)
			require.NoError(t, err)
			os.Stderr = saved

			out := buf.String()

			assert.Equal(t, tt.wantCode, code)
			for _, want := range tt.wantStderrContains {
				assert.Contains(t, out, want)
			}
			if tt.wantCode == 0 {
				assert.NotContains(t, out, "error:")
			}
		})
	}
}

func TestMain_Subprocess(t *testing.T) {
	tests := map[string]struct {
		args               []string // args passed to the child main (after program name)
		env                []string
		wantExitNonZero    bool
		wantOutputContains []string
	}{
		"no args": {
			args:               []string{},
			wantExitNonZero:    true,
			wantOutputContains: []string{"Usage: qumo"},
		},
		"unknown command": {
			args:               []string{"badcmd"},
			wantExitNonZero:    true,
			wantOutputContains: []string{"unknown command", "Usage: qumo"},
		},
		"relay env validation error": {
			// cli.RunRelay fails fast when it can't load the TLS cert/key.
			// Point at a non-existent cert so the test doesn't depend on whether
			// local certs/server.crt exists.
			env:                []string{"CERT_FILE=nonexistent.crt", "KEY_FILE=nonexistent.key"},
			args:               []string{"relay"},
			wantExitNonZero:    true,
			wantOutputContains: []string{"failed to setup TLS", "error:"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out, exitErr := runChildMain(t, tt.env, tt.args...)

			if tt.wantExitNonZero {
				// Expect non-zero exit
				if exitErr == nil {
					t.Fatalf("expected child to exit non-zero, got success; output=%q", out)
				}
			} else {
				require.NoError(t, exitErr)
			}

			for _, want := range tt.wantOutputContains {
				assert.Contains(t, out, want)
			}
		})
	}
}

// runChildMain re-executes the test binary in a special child mode that calls
// main(). It returns combined stdout+stderr and any exec error.
func runChildMain(t *testing.T, extraEnv []string, args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// Use the current test binary and ask it to run only the helper test.
	cmdArgs := append([]string{"-test.run=TestMain_ChildProcess", "--"}, args...)
	cmd := exec.Command(exe, cmdArgs...)
	// Signal to the child that it should execute main().
	cmd.Env = append(os.Environ(), "QUOMO_TEST_MAIN=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// TestMain_ChildProcess runs inside the spawned child test binary. When the
// QUOMO_TEST_MAIN env var is set the child will call main() with the
// arguments provided after "--" on the command line and then exit.
func TestMain_ChildProcess(t *testing.T) {
	if os.Getenv("QUOMO_TEST_MAIN") != "1" {
		return // not the helper child; let the test runner handle normal tests
	}

	// Find the separator `--` and use arguments after it as program args.
	sep := "--"
	var progArgs []string
	for i, a := range os.Args {
		if a == sep && i+1 < len(os.Args) {
			progArgs = os.Args[i+1:]
			break
		}
	}

	// If there was no `--`, default to no extra args (simulate os.Args length 1)
	if progArgs == nil {
		progArgs = []string{}
	}

	// Build os.Args for main() (program name + progArgs)
	os.Args = append([]string{"qumo"}, progArgs...)
	main()
	// main() should call os.Exit; if it returns, fail the child test.
	t.Fatalf("main() returned unexpectedly")
}
