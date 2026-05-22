//go:build !windows

package tasks

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
)

func TestPrepareBindMountTargetParentsCreatesHomeParentsOnly(t *testing.T) {
	uid := os.Geteuid()
	gid := os.Getegid()
	t.Setenv("CLOUD_CLI_PROXY_WORKER_UID", strconvItoa(uid))
	t.Setenv("CLOUD_CLI_PROXY_WORKER_GID", strconvItoa(gid))

	homeDir := t.TempDir()
	sourceDir := t.TempDir()
	req := agentapi.HostActionRequest{
		BindMounts: []agentapi.BindMount{
			{
				Source: filepath.Join(sourceDir, "claude-projects"),
				Target: "/home/work/.claude/projects",
			},
			{
				Source: filepath.Join(sourceDir, "repo"),
				Target: "/home/work/eniac/tianyijie/dev",
			},
			{
				Source: filepath.Join(sourceDir, "outside"),
				Target: "/opt/outside",
			},
		},
	}

	if err := prepareBindMountTargetParents(req, homeDir, "/home/work"); err != nil {
		t.Fatalf("prepareBindMountTargetParents: %v", err)
	}

	for _, rel := range []string{".claude", "eniac", "eniac/tianyijie"} {
		assertDirOwner(t, filepath.Join(homeDir, rel), uid, gid)
	}
	for _, rel := range []string{".claude/projects", "eniac/tianyijie/dev", "opt"} {
		if _, err := os.Stat(filepath.Join(homeDir, rel)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to stay uncreated, stat err=%v", rel, err)
		}
	}
}

func TestPrepareBindMountTargetParentsRejectsSymlinkParent(t *testing.T) {
	uid := os.Geteuid()
	gid := os.Getegid()
	t.Setenv("CLOUD_CLI_PROXY_WORKER_UID", strconvItoa(uid))
	t.Setenv("CLOUD_CLI_PROXY_WORKER_GID", strconvItoa(gid))

	homeDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(homeDir, "eniac")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	req := agentapi.HostActionRequest{
		BindMounts: []agentapi.BindMount{
			{
				Source: filepath.Join(t.TempDir(), "repo"),
				Target: "/home/work/eniac/tianyijie/dev",
			},
		},
	}

	if err := prepareBindMountTargetParents(req, homeDir, "/home/work"); err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
}

func assertDirOwner(t *testing.T, dir string, uid, gid int) {
	t.Helper()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s did not return syscall.Stat_t", dir)
	}
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("%s owner = %d:%d, want %d:%d", dir, stat.Uid, stat.Gid, uid, gid)
	}
}

func strconvItoa(value int) string {
	return strconv.Itoa(value)
}
