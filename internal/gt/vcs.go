// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gt

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type vcsNewer func(h *HttpHandler) VcsHandle

type HttpHandler struct {
	runner
	httpAddr string
	vcsAddr  string
	vcsName  string
	pkg      string
	l        net.Listener
	g        *GopathTest
	newer    vcsNewer

	handles map[string]VcsHandle
}

func (h *HttpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	out := w

	const templ = `<html><head><meta name="go-import" content="%s %s %s"></head></html>
`
	p := strings.TrimPrefix(r.URL.Path, "/")
	var handle VcsHandle
	for _, try := range h.handles {
		if strings.HasPrefix(p, try.pkg()) {
			handle = try
			break
		}
	}
	if handle == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}

	fmt.Fprintf(out, templ, h.httpAddr+"/"+handle.pkg(), h.vcsName, h.vcsAddr+handle.pkg()+"/.git")
}

func (h *HttpHandler) Close() error {
	return h.l.Close()
}
func (h *HttpHandler) HttpAddr() string {
	return h.httpAddr
}

// Setup returns type with Remove function that can be defer'ed.
func (h *HttpHandler) Setup() VcsHandle {
	vcs := h.newer(h)
	vcs.create()
	h.g.onClean(vcs.remove)

	h.handles[vcs.pkg()] = vcs
	return vcs
}

func NewHttpHandler(g *GopathTest, vcsName string) *HttpHandler {
	// Test if git is installed. If it is, enable the git test.
	// If enabled, start the http server and accept git server registrations.
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil
	}

	h := &HttpHandler{
		runner: runner{
			cwd: g.Current(),
			t:   g,
		},
		pkg:      g.pkg,
		vcsName:  vcsName,
		httpAddr: l.Addr().String(),
		l:        l,
		g:        g,

		handles: make(map[string]VcsHandle, 6),
	}
	go func() {
		err = http.Serve(l, h)
		if err != nil {
			fmt.Printf("Error serving HTTP server %v\n", err)
			os.Exit(1)
		}
	}()

	execPath, _ := exec.LookPath(vcsName)
	if len(execPath) == 0 {
		g.Skip("unsupported vcs")
	}
	h.execPath = execPath
	switch vcsName {
	default:
		panic("unknown vcs type")
	case "git":
		port := h.freePort()
		h.vcsAddr = fmt.Sprintf("git://localhost:%d/", port)

		h.runAsync(" Ready ", "daemon",
			"--listen=localhost", fmt.Sprintf("--port=%d", port),
			"--export-all", "--verbose", "--informative-errors",
			"--base-path="+g.Path(""), h.cwd,
		)
		fmt.Printf("base-path %q, serve %q\n", g.Path(""), h.cwd)

		h.newer = func(h *HttpHandler) VcsHandle {
			return &gitVcsHandle{
				vcsCommon: vcsCommon{
					runner: runner{
						execPath: execPath,
						cwd:      h.g.Current(),
						t:        h.g,
					},
					h:          h,
					importPath: h.g.pkg,
				},
			}
		}
	}
	return h
}

type vcsCommon struct {
	runner
	importPath string

	h *HttpHandler
}

func (vcs *vcsCommon) pkg() string {
	return vcs.importPath
}

type VcsHandle interface {
	remove()
	pkg() string
	create()
	Commit() (rev string, commitTime string)
}

type gitVcsHandle struct {
	vcsCommon
}

func (vcs *gitVcsHandle) remove() {
	delete(vcs.h.handles, vcs.pkg())
}
func (vcs *gitVcsHandle) create() {
	vcs.run("init")
	vcs.run("config", "user.name", "tests")
	vcs.run("config", "user.email", "tests@govendor.io")
}

func (vcs *gitVcsHandle) Commit() (rev string, commitTime string) {
	vcs.run("add", "-A")
	vcs.run("commit", "-a", "-m", "msg")
	out := vcs.run("show", "--pretty=format:%H@%ai", "-s")

	line := strings.TrimSpace(string(out))
	ss := strings.Split(line, "@")
	rev = ss[0]
	tm, err := time.Parse("2006-01-02 15:04:05 -0700", ss[1])
	if err != nil {
		panic("Failed to parse time: " + ss[1] + " : " + err.Error())
	}

	return rev, tm.UTC().Format(time.RFC3339)
}

type runner struct {
	execPath string
	cwd      string
	t        *GopathTest
}

func (r *runner) run(args ...string) []byte {
	cmd := exec.Command(r.execPath, args...)
	cmd.Dir = r.cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("Failed to run %q %q: %v", r.execPath, args, err)
	}
	return out
}

func (r *runner) freePort() int {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		r.t.Fatalf("Failed to find free port %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if runtime.GOOS == "windows" {
		time.Sleep(time.Millisecond * 300) // Wait for OS to release port.
	}
	return port
}

func (r *runner) runAsync(checkFor string, args ...string) *exec.Cmd {
	cmd := exec.Command(r.execPath, args...)
	cmd.Dir = r.t.Current()

	var buf *bytes.Buffer
	var bufErr *bytes.Buffer
	if checkFor != "" {
		buf = &bytes.Buffer{}
		bufErr = &bytes.Buffer{}
		cmd.Stdout = buf
		cmd.Stderr = bufErr
	}
	err := cmd.Start()
	if err != nil {
		r.t.Fatalf("Failed to start %q %q: %v", r.execPath, args)
	}
	r.t.onClean(func() {
		if cmd.Process == nil {
			return
		}
		cmd.Process.Signal(os.Interrupt)

		done := make(chan struct{}, 3)
		go func() {
			cmd.Process.Wait()
			done <- struct{}{}
		}()
		select {
		case <-time.After(time.Millisecond * 300):
			cmd.Process.Kill()
		case <-done:
		}
		r.t.Logf("%q StdOut: %s\n", cmd.Path, buf.Bytes())
		r.t.Logf("%q StdErr: %s\n", cmd.Path, bufErr.Bytes())
	})
	if checkFor != "" {
		for i := 0; i < 100; i++ {
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				r.t.Fatalf("unexpected stop %q %q\n%s\n%s\n", r.execPath, args, buf.Bytes(), bufErr.Bytes())
			}
			if strings.Contains(buf.String(), checkFor) {
				return cmd
			}
			if strings.Contains(bufErr.String(), checkFor) {
				return cmd
			}
			time.Sleep(time.Millisecond * 10)
		}
		r.t.Fatalf("failed to read expected output %q from %q %q\n%s\n", checkFor, r.execPath, args, bufErr.Bytes())
	}
	return cmd
}
