package irc

import (
	"os"
	"path/filepath"
	"testing"

	"gosuda.org/ivnp"
)

func TestLoadRouterConfigCreatesPrivateConfigAndParents(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "private", "router")
	path := filepath.Join(parent, "ivnp.conf")
	configuration, err := loadRouterConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Tunnel.Hops != 1 {
		t.Errorf("first-run hops = %d, want 1", configuration.Tunnel.Hops)
	}
	persisted, err := ivnp.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Tunnel.Hops != 1 {
		t.Errorf("persisted hops = %d, want 1", persisted.Tunnel.Hops)
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(root, "private"), 0700},
		{parent, 0700},
		{path, 0600},
	} {
		info, err := os.Stat(entry.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != entry.mode {
			t.Errorf("%s permissions = %04o, want %04o", entry.path, info.Mode().Perm(), entry.mode)
		}
	}
	if configuration.DataDir != "./data" || configuration.StatePath != filepath.Join("data", "state", "router.state") {
		t.Errorf("default router paths changed: data=%q state=%q", configuration.DataDir, configuration.StatePath)
	}
}

func TestLoadRouterConfigPreservesExistingSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		hops int
	}{
		{"empty", "", 1},
		{"omitted hops", "[tunnel]\n# hops = 7\nenabled = true\n[log]\nlevel = debug\n", 1},
		{"explicit upstream default", "[ tunnel ] ; operator policy\n hops = \"3\" # keep this choice\n", 3},
		{"explicit longer tunnels", "[tunnel]\nhops = 7\n", 7},
		{"quoted section in another value", "[log]\nlevel = info\n[paths]\ndata_dir = \"[tunnel];hops=7\"\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ivnp.conf")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			configuration, err := loadRouterConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if configuration.Tunnel.Hops != tc.hops {
				t.Errorf("hops = %d, want %d", configuration.Tunnel.Hops, tc.hops)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != tc.text {
				t.Errorf("existing configuration was rewritten: %q", contents)
			}
		})
	}
}

func TestLoadRouterConfigRejectsInvalidExplicitHops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ivnp.conf")
	const text = "[tunnel]\nhops = 0\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRouterConfig(path); err == nil {
		t.Fatal("invalid explicit hops accepted as an application default")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != text {
		t.Errorf("invalid configuration was replaced: %q", contents)
	}
}

func TestLoadRouterConfigRejectsSymlinkWithoutReplacingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "operator.conf")
	const text = "[tunnel]\nhops = 3\n"
	if err := os.WriteFile(target, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "ivnp.conf")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRouterConfig(path); err == nil {
		t.Fatal("symlink configuration accepted")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != text {
		t.Errorf("symlink target was replaced: %q", contents)
	}
}
