package container

import (
	"reflect"
	"testing"
)

func TestSub(t *testing.T) {
	tests := []struct {
		name     string
		in       []string
		wantSub  string
		wantRest []string
	}{
		{"docker run", []string{"docker", "run", "alpine", "ls"}, "run", []string{"alpine", "ls"}},
		{"podman path", []string{"/usr/bin/podman", "ps"}, "ps", []string{}},
		{"container prefix", []string{"docker", "container", "exec", "c", "sh"}, "exec", []string{"c", "sh"}},
		{"global flag", []string{"docker", "--debug", "run", "x"}, "run", []string{"x"}},
		{"global flag with value", []string{"docker", "-H", "tcp://h", "run", "x"}, "run", []string{"x"}},
		{"global long flag with value", []string{"docker", "--context", "c", "run"}, "run", []string{}},
		{"value flag at the end", []string{"docker", "-H"}, "", nil},
		{"only global flags", []string{"docker", "--debug"}, "", nil},
		{"container prefix alone", []string{"docker", "container"}, "", nil},
		{"bare docker", []string{"docker"}, "", nil},
		{"not a container tool", []string{"ls", "run"}, "", nil},
		{"empty", nil, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, rest := Sub(tt.in)
			if sub != tt.wantSub || !reflect.DeepEqual(rest, tt.wantRest) {
				t.Errorf("Sub(%q) = %q, %q; want %q, %q", tt.in, sub, rest, tt.wantSub, tt.wantRest)
			}
		})
	}
}

func TestCmd(t *testing.T) {
	tests := []struct {
		name       string
		in         []string
		wantInner  []string
		wantMounts []BindMount
	}{
		{"run with a command", []string{"docker", "run", "alpine", "rm", "-rf", "/x"}, []string{"rm", "-rf", "/x"}, nil},
		{"create and exec count too", []string{"docker", "create", "img", "ls"}, []string{"ls"}, nil},
		{"exec names the container", []string{"docker", "exec", "ctr", "ls"}, []string{"ls"}, nil},
		{"ps is not a run", []string{"docker", "ps"}, nil, nil},
		{"image without a command", []string{"docker", "run", "alpine"}, nil, nil},
		{"bare flags", []string{"docker", "run", "--rm", "-it", "alpine", "sh"}, []string{"sh"}, nil},
		{"-v bind mount", []string{"docker", "run", "-v", "/:/host", "alpine", "rm", "/host/x"}, []string{"rm", "/host/x"}, []BindMount{{"/", "/host"}}},
		{"-v attached", []string{"docker", "run", "-v/data:/d:ro", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/data", "/d"}}},
		{"-v at the end of a cluster", []string{"docker", "run", "-itv", "/a:/b", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/a", "/b"}}},
		{"--volume equals is split", []string{"docker", "run", "--volume=/a:/b", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/a", "/b"}}},
		{"--volume value", []string{"docker", "run", "--volume", "/a:/b", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/a", "/b"}}},
		{"named volume stays unmapped", []string{"docker", "run", "-v", "vol:/data", "alpine", "ls"}, []string{"ls"}, nil},
		{"anonymous volume", []string{"docker", "run", "-v", "/data", "alpine", "ls"}, []string{"ls"}, nil},
		{"relative and tilde and var hosts", []string{"docker", "run", "-v", "./a:/x", "-v", "~/b:/y", "-v", "$H:/z", "alpine", "ls"}, []string{"ls"}, []BindMount{{"./a", "/x"}, {"~/b", "/y"}, {"$H", "/z"}}},
		{"container path must be absolute", []string{"docker", "run", "-v", "/a:rel", "alpine", "ls"}, []string{"ls"}, nil},
		{"trailing slash on target trimmed", []string{"docker", "run", "-v", "/a:/b/", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/a", "/b"}}},
		{"--mount bind", []string{"docker", "run", "--mount", "type=bind,source=/s,target=/t", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/s", "/t"}}},
		{"--mount equals form with src and dst", []string{"docker", "run", "--mount=type=bind,src=/s,dst=/t", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/s", "/t"}}},
		{"--mount destination, no type", []string{"docker", "run", "--mount", "source=/s,destination=/t", "alpine", "ls"}, []string{"ls"}, []BindMount{{"/s", "/t"}}},
		{"--mount tmpfs ignored", []string{"docker", "run", "--mount", "type=tmpfs,target=/t", "alpine", "ls"}, []string{"ls"}, nil},
		{"--mount volume ignored", []string{"docker", "run", "--mount", "type=volume,source=v,target=/t", "alpine", "ls"}, []string{"ls"}, nil},
		{"entrypoint leads the command", []string{"docker", "run", "--entrypoint", "/bin/sh", "alpine", "-c", "id"}, []string{"/bin/sh", "-c", "id"}, nil},
		{"entrypoint without a command", []string{"docker", "run", "--entrypoint", "rm", "alpine"}, []string{"rm"}, nil},
		{"option values are not the image", []string{"docker", "run", "-e", "A=b", "--name", "n", "-w", "/w", "alpine", "ls"}, []string{"ls"}, nil},
		{"-- ends the options", []string{"docker", "run", "--", "alpine", "ls"}, []string{"ls"}, nil},
		{"bare dash is the image", []string{"docker", "run", "-", "ls"}, []string{"ls"}, nil},
		{"value option at the end", []string{"docker", "run", "-v"}, nil, nil},
		{"mounts survive a missing command", []string{"docker", "run", "-v", "/a:/b", "alpine"}, nil, []BindMount{{"/a", "/b"}}},
		{"not a container tool", []string{"rm", "-rf", "/"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner, mounts := Cmd(tt.in)
			if !reflect.DeepEqual(inner, tt.wantInner) || !reflect.DeepEqual(mounts, tt.wantMounts) {
				t.Errorf("Cmd(%q) = %q, %v; want %q, %v", tt.in, inner, mounts, tt.wantInner, tt.wantMounts)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	outer := &MountSet{Binds: []BindMount{{"/outer", "/o"}}}
	inner := []BindMount{{"/inner", "/i"}}

	got := (*MountSet)(nil).Within(inner)
	if want := (&MountSet{Binds: inner}); !reflect.DeepEqual(got, want) {
		t.Errorf("host.Within = %+v, want %+v", got, want)
	}

	got = outer.Within(inner)
	if want := []BindMount{{"/inner", "/i"}, {"/outer", "/o"}}; !reflect.DeepEqual(got.Binds, want) {
		t.Errorf("nested Within = %+v, want inner mounts before outer ones: %+v", got.Binds, want)
	}
	if len(outer.Binds) != 1 || len(inner) != 1 {
		t.Error("Within must not modify its inputs")
	}

	// A caller appending to its mounts slice must not reach into the result.
	spare := make([]BindMount, 1, 4)
	spare[0] = BindMount{"/s", "/s"}
	got = outer.Within(spare)
	_ = append(spare, BindMount{"/leak", "/leak"})
	if len(got.Binds) != 2 || got.Binds[1] != (BindMount{"/outer", "/o"}) {
		t.Errorf("Within shared capacity with its argument: %+v", got.Binds)
	}
}

func TestMapPath(t *testing.T) {
	m := &MountSet{Binds: []BindMount{
		{"/", "/host"},
		{"/home/u", "/home"},
		{"/home/u/deep", "/home/deep"},
		{"/srv", "/a/b"},
	}}
	tests := []struct{ in, want string }{
		{"relative/path", "relative/path"},
		{"-flag", "-flag"},
		{"/host", "/"},
		{"/host/etc/passwd", "/etc/passwd"},
		{"/home/x", "/home/u/x"},
		{"/home/deep/y", "/home/u/deep/y"}, // the longest matching target wins
		{"/homework", "ctr:/homework"},     // a prefix of the name is not a mount
		{"/etc/passwd", "ctr:/etc/passwd"},
		{"/", "/"},   // above every mount target: reaches the host
		{"/*", "/*"}, // glob above a mount
		{"/a", "/a"}, // above /a/b
		{"/a/*", "/a/*"},
		{"/a/c", "ctr:/a/c"},
		{"/host/../etc", "/etc"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := m.MapPath(tt.in); got != tt.want {
				t.Errorf("MapPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	empty := &MountSet{}
	if got := empty.MapPath("/etc"); got != "ctr:/etc" {
		t.Errorf("no mounts: MapPath(/etc) = %q", got)
	}
	if got := empty.MapPath("/"); got != "ctr:/" {
		t.Errorf("no mounts: MapPath(/) = %q", got)
	}
}
