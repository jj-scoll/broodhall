package main

// Commands run inside a container: `docker|podman [container] run|create|exec [opts] IMAGE
// cmd...` hands cmd to the container, so the rules read it like a bare command. The -v and
// --mount bind mounts map container paths back to host paths first, so `rm -rf /host` with
// `-v /:/host` is the rm of / it really is. Named volumes, tmpfs and anonymous volumes hold
// no host path and stay unmapped. Any other absolute path is the container's own (its /etc,
// its /), not the host's, so rules about host paths must not see it.

import (
	"path"
	"strings"
)

// bindMount is one host path mounted at a container path.
type bindMount struct{ Host, Ctr string }

// mountSet is the filesystem a command runs on when it runs in a container; nil is the host.
type mountSet struct{ Binds []bindMount }

// within is the filesystem of a container started with mounts inside m.
func (m *mountSet) within(mounts []bindMount) *mountSet {
	if m == nil {
		return &mountSet{mounts}
	}
	return &mountSet{append(mounts[:len(mounts):len(mounts)], m.Binds...)}
}

// containerRunValueOpts are the docker/podman run|create|exec options that take a separate
// value; every other option is a bare flag.
var containerRunValueOpts = func() map[string]bool {
	m := map[string]bool{}
	for _, o := range []string{"add-host", "annotation", "attach", "blkio-weight", "blkio-weight-device", "cap-add", "cap-drop",
		"cgroup-parent", "cgroupns", "cgroups", "cidfile", "cpu-period", "cpu-quota", "cpu-rt-period", "cpu-rt-runtime", "cpu-shares",
		"cpus", "cpuset-cpus", "cpuset-mems", "detach-keys", "device", "device-cgroup-rule", "device-read-bps", "device-read-iops",
		"device-write-bps", "device-write-iops", "dns", "dns-option", "dns-opt", "dns-search", "domainname", "entrypoint", "env",
		"env-file", "expose", "gidmap", "gpus", "group-add", "health-cmd", "health-interval", "health-retries", "health-start-period",
		"health-timeout", "hostname", "ip", "ip6", "ipc", "isolation", "label", "label-file", "link", "link-local-ip", "log-driver",
		"log-opt", "mac-address", "memory", "memory-reservation", "memory-swap", "memory-swappiness", "mount", "name", "network",
		"net", "network-alias", "oom-score-adj", "pid", "pids-limit", "platform", "pod", "publish", "pull", "restart", "runtime",
		"secret", "security-opt", "shm-size", "stop-signal", "stop-timeout", "storage-opt", "sysctl", "tmpfs", "tz", "uidmap",
		"ulimit", "user", "userns", "uts", "volume", "volume-driver", "volumes-from", "workdir"} {
		m["--"+o] = true
	}
	return m
}()

// containerRunShortValue are the short options that take a value (-v, -e, -w, ...), also
// attached or at the end of a cluster (-itv /:/host, -v/x:/y).
const containerRunShortValue = "acehlmpuvw"

// containerSub returns the docker|podman subcommand of w (run, exec, ps, ...), skipping
// global options and the `container` management prefix, and the words after it.
func containerSub(w []string) (sub string, rest []string) {
	if len(w) == 0 || path.Base(w[0]) != "docker" && path.Base(w[0]) != "podman" {
		return "", nil
	}
	args := w[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		skip := 1
		if containerValueOpts[args[0]] {
			skip = 2
		}
		if skip > len(args) {
			skip = len(args)
		}
		args = args[skip:]
	}
	if len(args) > 0 && args[0] == "container" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", nil
	}
	return args[0], args[1:]
}

// containerCmd returns the command a docker|podman run|create|exec starts in the container
// (the words after IMAGE, led by --entrypoint when set) and the bind mounts it declares.
// inner is nil when w is not such a command or starts none.
func containerCmd(w []string) (inner []string, mounts []bindMount) {
	sub, rest := containerSub(w)
	if sub != "run" && sub != "create" && sub != "exec" {
		return nil, nil
	}
	entrypoint := ""
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			i++ // the image follows
		} else if strings.HasPrefix(a, "-") && a != "-" {
			opt, val, takes := a, "", false
			if strings.HasPrefix(a, "--") {
				var hasEq bool
				if opt, val, hasEq = strings.Cut(a, "="); !hasEq {
					takes = containerRunValueOpts[opt]
				}
			} else {
				for j := 1; j < len(a); j++ {
					if strings.IndexByte(containerRunShortValue, a[j]) >= 0 {
						opt = "-" + a[j:j+1]
						if val = a[j+1:]; val == "" {
							takes = true
						}
						break
					}
				}
			}
			if takes && i+1 < len(rest) {
				i++
				val = rest[i]
			}
			switch opt {
			case "-v", "--volume":
				mounts = appendVolume(mounts, val)
			case "--mount":
				mounts = appendMount(mounts, val)
			case "--entrypoint":
				entrypoint = val
			}
			continue
		}
		// rest[i] is the image (the container for exec); the command follows.
		if i >= len(rest) {
			return nil, mounts
		}
		inner = rest[i+1:]
		if entrypoint != "" {
			inner = append([]string{entrypoint}, inner...)
		}
		if len(inner) == 0 {
			return nil, mounts
		}
		return inner, mounts
	}
	return nil, mounts
}

// hostPathLike tells a bind source from a named volume: named volumes have no slash.
func hostPathLike(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, "$") || strings.HasPrefix(s, ".")
}

func addBind(ms []bindMount, host, ctr string) []bindMount {
	ctr = strings.TrimRight(ctr, "/")
	if hostPathLike(host) && strings.HasPrefix(ctr, "/") {
		ms = append(ms, bindMount{host, ctr})
	}
	return ms
}

// appendVolume adds -v host:ctr[:opts].
func appendVolume(ms []bindMount, v string) []bindMount {
	parts := strings.Split(v, ":")
	if len(parts) < 2 {
		return ms
	}
	return addBind(ms, parts[0], parts[1])
}

// appendMount adds --mount type=bind,source=...,target=... (src, dst and destination too).
func appendMount(ms []bindMount, v string) []bindMount {
	var typ, host, ctr string
	for _, kv := range strings.Split(v, ",") {
		k, val, _ := strings.Cut(kv, "=")
		switch k {
		case "type":
			typ = val
		case "source", "src":
			host = val
		case "target", "destination", "dst":
			ctr = val
		}
	}
	if typ != "bind" && typ != "" {
		return ms
	}
	return addBind(ms, host, ctr)
}

// mapPath rewrites an absolute container path under the longest matching mount target to its
// host path. A path above a mount target (/, /*) reaches the host through the mount, so it
// stays as written. Any other absolute path is the container's own and gets a "ctr:" prefix.
func (m *mountSet) mapPath(w string) string {
	if !strings.HasPrefix(w, "/") {
		return w
	}
	best := -1
	for i, b := range m.Binds {
		if (w == b.Ctr || strings.HasPrefix(w, b.Ctr+"/")) && (best < 0 || len(b.Ctr) > len(m.Binds[best].Ctr)) {
			best = i
		}
	}
	if best >= 0 {
		return path.Join(m.Binds[best].Host, strings.TrimPrefix(w, m.Binds[best].Ctr))
	}
	above := strings.TrimRight(strings.TrimSuffix(w, "*"), "/")
	for _, b := range m.Binds {
		if strings.HasPrefix(b.Ctr, above+"/") {
			return w
		}
	}
	return "ctr:" + w
}

// mapCmd maps the words and redirect targets of c, which runs on m, to host paths.
func mapCmd(c simpleCmd, m *mountSet) simpleCmd {
	if m == nil {
		return c
	}
	words := make([]string, len(c.Words))
	for i, w := range c.Words {
		words[i] = m.mapPath(w)
	}
	redirs := make([]astRedir, len(c.Redirs))
	for i, r := range c.Redirs {
		redirs[i] = astRedir{r.Op, m.mapPath(r.Target)}
	}
	c.Words, c.Redirs = words, redirs
	return c
}
