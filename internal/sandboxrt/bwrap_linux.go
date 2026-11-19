// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package sandboxrt

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// Bubblewrap is the backend that runs commands with bubblewrap, in new
// user, PID, IPC, UTS, cgroup, mount and network namespaces. With the
// egress boundary, the launcher runs first in the sandbox, as in a Docker
// container: it gives the sandbox its only route, mounts the live
// workspaces, drops every capability and installs a seccomp filter, and
// then starts the command. Without it, the sandbox has only loopback, or
// the network of this machine with BubblewrapConfig.HostNetwork.
type Bubblewrap struct {
	cfg BubblewrapConfig

	// probeMu serializes the checks of the launcher's setup, which run
	// once for the backend, without and with live workspaces.
	probeMu sync.Mutex
	probes  [2]probeResult
}

var _ Backend = (*Bubblewrap)(nil)

// NewBubblewrap returns the bubblewrap backend of cfg.
func NewBubblewrap(cfg BubblewrapConfig) *Bubblewrap { return &Bubblewrap{cfg: cfg} }

// bwrapTimeout bounds the check that bwrap can make a sandbox.
const bwrapTimeout = DockerTimeout

// CheckBubblewrap checks that bwrap is installed and can make the
// namespaces of a sandbox, which needs unprivileged user namespaces.
func CheckBubblewrap(ctx context.Context) error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return &BubblewrapError{Problem: "is not installed", Fix: "install bubblewrap", Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, bwrapTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bwrap", "--unshare-all", "--die-with-parent", "--ro-bind", "/", "/", "true").CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return &BubblewrapError{Problem: fmt.Sprintf("didn't make a sandbox within %s", bwrapTimeout), Fix: "check that bwrap works on this machine"}
	}
	return &BubblewrapError{Problem: "can't make a sandbox on this machine", Fix: "allow unprivileged user namespaces", Err: fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))}
}

// Probe implements [Backend]: bwrap makes a sandbox, and whether the
// egress boundary and live workspaces can work, which it checks by running
// the launcher's check, since a user namespace may lack what they take.
func (b *Bubblewrap) Probe(ctx context.Context) (Capabilities, error) {
	if err := CheckBubblewrap(ctx); err != nil {
		return Capabilities{}, err
	}
	var c Capabilities
	launcher, err := b.launcherPath()
	switch {
	case err != nil:
		c.Boundary = &boundaryError{why: err.Error()}
	case CheckTUN() != nil:
		c.Boundary = &boundaryError{why: CheckTUN().Error()}
	default:
		if err := checkSetup(ctx, launcher, false); err != nil {
			c.Boundary = &boundaryError{why: err.Error()}
		}
	}
	if c.Boundary != nil {
		c.Served = fmt.Errorf("a live workspace needs the egress boundary, whose launcher mounts it: %w", c.Boundary)
		return c, nil
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		c.Served = fmt.Errorf("this machine has no FUSE device: %w", err)
	} else if err := checkSetup(ctx, launcher, true); err != nil {
		c.Served = err
		if sme, ok := errors.AsType[*ServedMountError](err); ok {
			c.Served = errors.New("bubblewrap can't mount a live workspace here: " + sme.Reason)
		}
	}
	return c, nil
}

// checkSetup runs the launcher's check in a sandbox with an egress
// directory of its own, and, if served, a live workspace of an empty tree.
func checkSetup(ctx context.Context, launcher string, served bool) error {
	dir, err := os.MkdirTemp("", "a2a-bwrap-")
	if err != nil {
		return err
	}
	run := &bwrapRun{system: hostSystemView().args, launcher: launcher, egressDir: dir}
	closeFn := func() error { return nil }
	if served {
		mounts, closeServed, err := serveTrees([]ServedMount{{Dst: "/probe", Tree: fsys.Memory()}}, dir)
		if err != nil {
			return errors.Join(err, os.RemoveAll(dir))
		}
		run.served, closeFn = mounts, closeServed
	}
	err = run.check(ctx)
	return errors.Join(err, closeFn(), os.RemoveAll(dir))
}

// launcherPath returns the launcher that the sandboxes run.
func (b *Bubblewrap) launcherPath() (string, error) {
	if b.cfg.Launcher != nil {
		if l := b.cfg.Launcher(); l != "" {
			return CheckLauncher(l)
		}
	}
	return Launcher()
}

// Prepare implements [Backend]. With a boundary, it opens it, serves the
// live workspaces, and checks that the launcher can set up the sandbox,
// once for the backend.
func (b *Bubblewrap) Prepare(ctx context.Context, spec Spec) (Sandbox, error) {
	if len(spec.Expose) > 1 {
		return nil, errors.New("a bubblewrap sandbox exposes at most one socket")
	}
	if len(spec.Served) > 0 && spec.Network.Boundary == nil {
		return nil, &ServedMountError{Dst: spec.Served[0].Dst, Reason: "a live workspace needs the egress boundary, whose launcher mounts it"}
	}
	if err := CheckBubblewrap(ctx); err != nil {
		return nil, err
	}
	view := hostSystemView()
	cmdPath, programDirs, err := hostPrograms(spec.Command, b.cfg.Programs, view)
	if err != nil {
		return nil, err
	}
	if err := fsutil.MkdirPrivate(b.cfg.Dir); err != nil {
		return nil, fmt.Errorf("creating the directory of the sandboxes' files: %w", err)
	}
	dir, err := os.MkdirTemp(b.cfg.Dir, "bwrap-")
	if err != nil {
		return nil, fmt.Errorf("creating the directory of the sandbox's files: %w", err)
	}
	run := &bwrapRun{
		system:      view.args,
		hostNetwork: b.cfg.HostNetwork,
		mounts:      spec.Mounts,
		masks:       spec.Masks,
		scratch:     spec.Scratch,
		home:        spec.Home,
		programs:    programDirs,
		command:     append([]string{cmdPath}, spec.Command.Args...),
		dir:         spec.Command.Dir,
	}
	if len(spec.Expose) > 0 && spec.Expose[0].Path != "" {
		run.expose = spec.Expose[0].Path
	}
	env := append([]string{"PATH=" + bwrapPath(programDirs)}, withoutEgressCA(spec.Command.Env)...)
	if run.expose != "" && spec.Expose[0].Env != "" {
		env = append(env, spec.Expose[0].Env+"="+path.Join(exposeDir, filepath.Base(run.expose)))
	}
	closeAll := func() error { return os.RemoveAll(dir) }
	info := Info{Summary: "bubblewrap"}
	if spec.Network.Boundary != nil {
		closeEgress, err := b.openEgress(ctx, spec, run)
		if err != nil {
			return nil, errors.Join(err, closeAll())
		}
		closeAll = func() error { return errors.Join(closeEgress(), os.RemoveAll(dir)) }
		if len(spec.Network.CA) > 0 {
			env = append(env, boundary.EgressCAEnv(spec.Network.CA))
			if bundle := hostBundle(); bundle != "" {
				run.bundle = &bundleCopy{path: bundle, copy: filepath.Join(dir, "ca-bundle.pem")}
			}
		}
		info.Boundary = true
	}
	env = append(env, "HOME="+ContainerHome)
	return &bwrapSandbox{run: run, env: env, opts: Options{Stderr: spec.Stderr, Check: spec.Check}, info: info, close: sync.OnceValue(closeAll)}, nil
}

// openEgress opens the boundary of the sandbox of run, serves its live
// workspaces, and checks the launcher's setup. It returns the function
// that closes them.
func (b *Bubblewrap) openEgress(ctx context.Context, spec Spec, run *bwrapRun) (func() error, error) {
	launcher, err := b.launcherPath()
	if err != nil {
		return nil, &boundaryError{why: err.Error()}
	}
	if err := CheckTUN(); err != nil {
		return nil, &boundaryError{why: err.Error()}
	}
	ep, err := spec.Network.Boundary(noHostGateway, false)
	if err != nil {
		return nil, fmt.Errorf("opening the egress socket: %w", err)
	}
	closeFn := ep.Close
	run.launcher, run.egressDir = launcher, filepath.Dir(ep.Socket)
	if len(spec.Served) > 0 {
		served, closeServed, err := serveTrees(spec.Served, run.egressDir)
		if err != nil {
			return nil, errors.Join(err, closeFn())
		}
		run.served = served
		closeEgress := closeFn
		closeFn = func() error { return errors.Join(closeServed(), closeEgress()) }
	}
	if err := b.probeOnce(len(spec.Served) > 0, func() error { return run.check(ctx) }); err != nil {
		return nil, errors.Join(err, closeFn())
	}
	return closeFn, nil
}

// noHostGateway is the host gateway of a bubblewrap sandbox, which has
// none.
func noHostGateway(context.Context, uint16) (netip.AddrPort, bool) { return netip.AddrPort{}, false }

// probeOnce runs probe, the check of the launcher's setup, with live
// workspaces if served, and keeps its result for every later call of the
// same kind once it passed or found that the setup can't work on this
// machine.
func (b *Bubblewrap) probeOnce(served bool, probe func() error) error {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	res := &b.probes[0]
	if served {
		res = &b.probes[1]
	}
	if res.done {
		return res.err
	}
	err := probe()
	_, mountFailed := errors.AsType[*ServedMountError](err)
	if err == nil || errors.Is(err, ErrNoBoundary) || mountFailed {
		res.err, res.done = err, true
	}
	return err
}

// systemDirs are the directories of this machine that a bubblewrap sandbox
// shows read-only, as links where they are links, as on systems that
// merged them into /usr, except the home directories under them.
var systemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"}

// etcFiles are the entries of /etc that a bubblewrap sandbox shows
// read-only, where they exist: the users and groups, name resolution, the
// dynamic linker, the links of the alternatives that commands in /usr
// point to, the CA certificates, and the time zone. The rest of /etc,
// such as the host's credentials and keys, stays out.
var etcFiles = []string{
	"passwd", "group", "hosts", "host.conf", "nsswitch.conf", "gai.conf", "services", "protocols",
	"ld.so.cache", "ld.so.conf", "ld.so.conf.d", "alternatives",
	"ssl", "pki", "ca-certificates", "ca-certificates.conf", "crypto-policies",
	"localtime", "timezone", "os-release", "mime.types",
}

// systemView is what a sandbox shows of the system directories: the
// arguments of bwrap, and the paths they show, each with what's under it.
type systemView struct {
	args  []string
	shown []string
}

// newSystemView returns the view of dirs without homes. A directory that
// holds a home, such as /usr on machines that keep homes under
// /usr/local, shows its other entries, and so on down to the home, which
// it leaves out.
func newSystemView(dirs, homes []string) systemView {
	var v systemView
	holdsHome := func(d string) bool {
		return slices.ContainsFunc(homes, func(h string) bool { return h == d || strings.HasPrefix(h, d+"/") })
	}
	var add func(d string)
	add = func(d string) {
		if target, err := os.Readlink(d); err == nil {
			v.args = append(v.args, "--symlink", target, d)
			v.shown = append(v.shown, d)
			return
		}
		if !holdsHome(d) {
			v.args = append(v.args, "--ro-bind-try", d, d)
			v.shown = append(v.shown, d)
			return
		}
		if slices.Contains(homes, d) {
			return
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			add(filepath.Join(d, e.Name()))
		}
	}
	for _, d := range dirs {
		add(d)
	}
	return v
}

// shows reports whether v shows p.
func (v systemView) shows(p string) bool {
	return slices.ContainsFunc(v.shown, func(d string) bool { return p == d || strings.HasPrefix(p, d+"/") })
}

// userHomes returns the home directories of the user, as $HOME and the
// user database name them, with links resolved too.
func userHomes() []string {
	var homes []string
	add := func(h string) {
		if h == "" || !filepath.IsAbs(h) {
			return
		}
		for _, p := range []string{filepath.Clean(h), fsutil.ResolvePath(h)} {
			if !slices.Contains(homes, p) {
				homes = append(homes, p)
			}
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		add(h)
	}
	if u, err := user.Current(); err == nil {
		add(u.HomeDir)
	}
	return homes
}

// hostSystemView returns the view of this machine's system directories,
// which leaves out the user's home directories and, where a system
// directory holds them, the directories that hold the homes of others.
func hostSystemView() systemView {
	homes := userHomes()
	for _, h := range slices.Clone(homes) {
		if parent := filepath.Dir(h); parent != "/" && !slices.Contains(systemDirs, parent) && !slices.Contains(homes, parent) {
			homes = append(homes, parent)
		}
	}
	return newSystemView(systemDirs, homes)
}

// systemPath is the PATH of the system directories.
const systemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// bwrapPath returns the PATH of a sandbox: the directories in which this
// machine's PATH found the programs, then the system directories.
func bwrapPath(programs []programDir) string {
	system := strings.Split(systemPath, ":")
	var dirs []string
	for _, p := range programs {
		if p.onPath && !slices.Contains(dirs, p.dir) && !slices.Contains(system, p.dir) {
			dirs = append(dirs, p.dir)
		}
	}
	return strings.Join(append(dirs, systemPath), ":")
}

// programDir is a directory of a program of this machine that a sandbox
// shows read-only at its path.
type programDir struct {
	dir string
	// onPath is set for the directory in which PATH found the program.
	onPath bool
	// shown is set where the system view shows the directory already.
	shown bool
}

// hostPrograms finds the command of c and the programs names on this
// machine's PATH, and returns the path of the command and the directories
// of the programs: the one in which PATH found each, and the one of its
// file with links resolved. A command path with a slash that isn't
// absolute is in the sandbox, such as in the workspace.
func hostPrograms(c Command, names []string, view systemView) (string, []programDir, error) {
	var dirs []programDir
	add := func(d programDir) error {
		d.shown = view.shows(d.dir)
		broad, err := projectfiles.BroadMount(d.dir)
		if err != nil {
			return err
		}
		if broad {
			return errBroadProgramDir
		}
		if i := slices.IndexFunc(dirs, func(o programDir) bool { return o.dir == d.dir }); i >= 0 {
			dirs[i].onPath = dirs[i].onPath || d.onPath
			return nil
		}
		dirs = append(dirs, d)
		return nil
	}
	find := func(name string) (string, error) {
		found, err := exec.LookPath(name)
		if err != nil {
			return "", &ProgramError{Name: name, Err: err}
		}
		abs, err := filepath.Abs(found)
		if err != nil {
			return "", &ProgramError{Name: name, Err: err}
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", &ProgramError{Name: name, Err: err}
		}
		if err := add(programDir{dir: filepath.Dir(abs), onPath: !strings.ContainsRune(name, '/')}); err != nil {
			return "", &ProgramError{Name: name, Err: err}
		}
		if err := add(programDir{dir: filepath.Dir(real)}); err != nil {
			return "", &ProgramError{Name: name, Err: err}
		}
		return real, nil
	}
	cmdPath := c.Path
	if !strings.ContainsRune(c.Path, '/') || filepath.IsAbs(c.Path) {
		var err error
		if cmdPath, err = find(c.Path); err != nil {
			return "", nil, err
		}
	}
	for _, name := range names {
		if _, err := find(name); err != nil {
			return "", nil, err
		}
	}
	return cmdPath, dirs, nil
}

// hostBundle returns the system CA bundle of this machine, or "".
func hostBundle() string {
	for _, p := range boundary.SystemBundles {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// bundleCopy is a copy of the system CA bundle, made at each start, which
// the sandbox shows writable in its place so that the launcher can add
// the egress CA to it.
type bundleCopy struct {
	path, copy string
}

// bwrapRun is what the arguments of bwrap are built from.
type bwrapRun struct {
	// system are the arguments that show the system directories.
	system []string
	// hostNetwork shares this machine's network with a sandbox without the
	// egress boundary.
	hostNetwork bool
	mounts      []Mount
	masks       []Mask
	scratch     []string
	home        string
	programs    []programDir
	// expose is the host path of the socket that the command reaches.
	expose string
	// launcher is the host path of the launcher, and egressDir the
	// directory of the egress socket, with a boundary. served are the live
	// workspaces that the launcher mounts.
	launcher, egressDir string
	served              []boundary.Mount
	bundle              *bundleCopy
	command             []string
	dir                 string
}

// mountOp is the arguments of bwrap that make one mount at dst. Masks go
// after the mounts at the same path.
type mountOp struct {
	dst  string
	mask bool
	args []string
}

// args returns the arguments of bwrap. With check, the launcher only
// checks its setup.
func (r *bwrapRun) args(check bool) []string {
	behind := r.launcher != ""
	args := []string{"--unshare-all", "--die-with-parent", "--new-session"}
	if behind {
		// The launcher starts as uid 0 of the sandbox's user namespace,
		// which is the user who runs it, with what building the network
		// and mounting the live workspaces take, and drops it all before
		// the command starts.
		// It runs as PID 1, so that no process of the sandbox is without its
		// seccomp filter.
		args = append(args, "--as-pid-1", "--uid", "0", "--gid", "0", "--cap-add", "CAP_NET_ADMIN", "--cap-add", "CAP_SETPCAP")
		if len(r.served) > 0 {
			args = append(args, "--cap-add", "CAP_SYS_ADMIN")
		}
	} else if r.hostNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, r.system...)
	args = append(args, "--tmpfs", "/etc")
	for _, f := range etcFiles {
		args = append(args, "--ro-bind-try", "/etc/"+f, "/etc/"+f)
	}
	if !behind && r.hostNetwork {
		// The launcher points the sandbox at its own resolver.
		args = append(args, "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf")
	}
	if r.bundle != nil {
		args = append(args, "--bind", r.bundle.copy, r.bundle.path)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/var", "--tmpfs", "/var/tmp", "--tmpfs", "/run")
	if behind {
		args = append(args, "--dev-bind", tunDevice, tunDevice)
		if len(r.served) > 0 {
			args = append(args, "--dev-bind", "/dev/fuse", "/dev/fuse")
		}
		args = append(args, "--ro-bind", r.launcher, boundary.ContainerLauncher, "--ro-bind", r.egressDir, boundary.ContainerSocketDir)
	}
	if r.expose != "" {
		args = append(args, "--ro-bind", filepath.Dir(r.expose), exposeDir)
	}
	for _, op := range r.mountOps() {
		args = append(args, op.args...)
	}
	if r.dir != "" {
		args = append(args, "--chdir", r.dir)
	}
	args = append(args, "--")
	if !behind {
		return append(args, r.command...)
	}
	args = append(args, boundary.ContainerLauncher, "sandbox-init")
	if check {
		args = append(args, "--check")
	}
	args = append(args, "--confine")
	for _, m := range r.served {
		args = append(args, "--fs", boundary.MountArg(m))
	}
	args = append(args, boundary.ContainerSocketDir+"/"+boundary.SocketName, "0", "0")
	if check {
		return args
	}
	return append(args, r.command...)
}

// mountOps returns the mounts of the programs, the Spec and the home, in
// the order bwrap makes them: a path before the paths under it, and the
// masks after the mounts at the same path.
func (r *bwrapRun) mountOps() []mountOp {
	var ops []mountOp
	for _, p := range r.programs {
		if !p.shown {
			ops = append(ops, mountOp{dst: p.dir, args: []string{"--ro-bind", p.dir, p.dir}})
		}
	}
	for _, m := range r.mounts {
		flag := "--bind"
		if m.ReadOnly {
			flag = "--ro-bind"
		}
		ops = append(ops, mountOp{dst: m.Dst, args: []string{flag, m.Src, m.Dst}})
	}
	for _, m := range r.served {
		ops = append(ops, mountOp{dst: m.Dst, args: []string{"--dir", m.Dst}})
	}
	for _, dst := range r.scratch {
		ops = append(ops, mountOp{dst: dst, args: []string{"--tmpfs", dst}})
	}
	if r.home != "" {
		ops = append(ops, mountOp{dst: ContainerHome, args: []string{"--bind", r.home, ContainerHome}})
	} else {
		ops = append(ops, mountOp{dst: ContainerHome, args: []string{"--dir", ContainerHome}})
	}
	for _, m := range r.masks {
		if m.Src != "" {
			ops = append(ops, mountOp{dst: m.Dst, mask: true, args: []string{"--ro-bind", m.Src, m.Dst}})
			continue
		}
		ops = append(ops, mountOp{dst: m.Dst, mask: true, args: []string{"--tmpfs", m.Dst, "--remount-ro", m.Dst}})
	}
	slices.SortStableFunc(ops, func(a, b mountOp) int {
		if c := cmp.Compare(depth(a.dst), depth(b.dst)); c != 0 {
			return c
		}
		switch {
		case a.mask == b.mask:
			return 0
		case a.mask:
			return 1
		}
		return -1
	})
	return ops
}

// depth returns the number of elements of the clean absolute path p.
func depth(p string) int {
	p = path.Clean(p)
	if p == "/" {
		return 0
	}
	return strings.Count(p, "/")
}

// check runs the launcher in a sandbox of r that only sets up the network
// and the mounts and drops its capabilities. It returns an
// [*EgressSetupError] when the setup can't work on this machine.
func (r *bwrapRun) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bwrap", r.args(true)...)
	cmd.Env = []string{"PATH=" + systemPath}
	cmd.Dir = "/"
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("the check of the sandbox's setup didn't finish: %w", ctx.Err())
	}
	ee, exited := errors.AsType[*exec.ExitError](err)
	switch {
	case exited && len(r.served) > 0 && ee.ExitCode() == boundary.TreeFailedExit:
		return &ServedTreeError{Dst: r.served[0].Dst, Reason: launcherReason(string(out), boundary.TreeErrorPrefix)}
	case exited && len(r.served) > 0 && ee.ExitCode() == boundary.MountFailedExit:
		return &ServedMountError{Dst: r.served[0].Dst, Reason: mountReasonOf(string(out))}
	case exited && ee.ExitCode() == boundary.SetupFailedExit:
		return &EgressSetupError{Reason: "bubblewrap can't set up the sandbox's network here: " + reasonOf(string(out))}
	}
	return fmt.Errorf("checking the setup of the bubblewrap sandbox: %w: %s", err, firstLine(strings.TrimSpace(string(out))))
}

type bwrapSandbox struct {
	run  *bwrapRun
	env  []string
	opts Options
	info Info
	// close closes the egress boundary and removes the sandbox's files.
	close func() error
}

func (s *bwrapSandbox) Start(context.Context) (io.ReadWriteCloser, error) {
	if err := s.opts.check(); err != nil {
		return nil, err
	}
	if b := s.run.bundle; b != nil {
		data, err := os.ReadFile(b.path)
		if err != nil {
			return nil, fmt.Errorf("copying the CA bundle of this machine: %w", err)
		}
		if err := os.WriteFile(b.copy, data, 0o600); err != nil {
			return nil, fmt.Errorf("copying the CA bundle of this machine: %w", err)
		}
	}
	cmd := exec.Command("bwrap", s.run.args(false)...)
	cmd.Env = s.env
	cmd.Dir = "/"
	return startProcess(cmd, s.opts.Stderr)
}

func (s *bwrapSandbox) Close(context.Context) error { return s.close() }

func (s *bwrapSandbox) Info() Info { return s.info }
