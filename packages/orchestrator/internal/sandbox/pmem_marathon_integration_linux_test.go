//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
)

// Explicit target-only gate: one replay VM at a time, with the real production
// Factory/Pause/upload/restore path and an unchanged external trace/replay.
func TestPmemMarathonIntegration(t *testing.T) { //nolint:paralleltest
	if os.Getenv("E2B_PMEM_INTEGRATION_DIR") == "" {
		t.Skip("requires isolated Linux KVM fixtures and E2B_MARATHON_TRACE/REPLAY/MTOOL")
	}
	t.Setenv("E2B_PMEM_MARATHON", "1")
	TestPmemFactoryIntegration(t)
}

func sharingMarathonReplay(t *testing.T, ctx context.Context, dir string, current *Sandbox,
	pause func(string, *Sandbox) string, resume func(string, string) *Sandbox,
) *Sandbox {
	t.Helper()
	started := time.Now()
	inputs := map[string]any{}
	read := func(key string) []byte {
		path := os.Getenv(key)
		require.True(t, filepath.IsAbs(path), "%s must be an absolute input path", key)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		digest := sha256.Sum256(data)
		inputs[key] = map[string]any{"path": path, "sha256": hex.EncodeToString(digest[:]), "size": len(data)}
		return data
	}
	trace, replay, mtool := read("E2B_MARATHON_TRACE"), read("E2B_MARATHON_REPLAY"), read("E2B_MARATHON_MTOOL")
	traceHash := fmt.Sprintf("%x", sha256.Sum256(trace))
	require.Equal(t, "683d7fcc17c4edbd56ec6977293d3ec527a0ed4edc9cb1aa8c0f9935a200dcc5", traceHash)
	var trajectory struct {
		Name  string `json:"name"`
		Turns []struct {
			Tool *struct {
				CallIDs []string `json:"call_ids"`
			} `json:"tool"`
		} `json:"turns"`
	}
	require.NoError(t, json.Unmarshal(trace, &trajectory))
	require.Len(t, trajectory.Turns, 1016)
	wantTools := 0
	var wantCalls []string
	for _, turn := range trajectory.Turns {
		if turn.Tool != nil {
			wantTools++
			wantCalls = append(wantCalls, turn.Tool.CallIDs...)
		}
	}
	sharingReceipt(t, filepath.Join(dir, "marathon-inputs.json"), inputs)
	for path, data := range map[string][]byte{
		"/workspace/trace.json": trace, "/workspace/replay.py": replay,
		"/usr/local/bin/memory-lab-mtool": mtool,
		"/workspace/replay-config.json":   []byte(`{"time_scale":0.001,"ws_mb":64,"checkpoint_turns":[10,200,700]}`),
	} {
		sharingMarathonUpload(t, ctx, current, path, data)
	}
	output, err := sharingExec(ctx, current, "chmod 755 /usr/local/bin/memory-lab-mtool && /usr/local/bin/memory-lab-mtool --policy")
	require.NoError(t, err)
	require.Contains(t, string(output), "fixed-work-no-padding-v1")
	sharingMarathonBacking(t, ctx, dir, "start", current)
	_, err = sharingExec(ctx, current, "truncate -s 16M /workspace/corpus.bin; python3 /workspace/replay.py > /workspace/replay.log 2>&1 < /dev/null & echo $! > /workspace/replay.pid")
	require.NoError(t, err)
	var guestPID int
	var checkpoints []map[string]any
	for _, turn := range []int{10, 200, 700} {
		var boundary struct {
			Turn   int    `json:"turn"`
			PID    int    `json:"pid"`
			SHA256 string `json:"sha256"`
		}
		for {
			data := sharingMarathonPoll(t, ctx, current, "/workspace/boundary.json")
			if len(data) != 0 && json.Unmarshal(data, &boundary) == nil && boundary.Turn == turn {
				break
			}
			sharingMarathonWait(t, ctx)
		}
		if guestPID == 0 {
			guestPID = boundary.PID
		}
		require.Equal(t, guestPID, boundary.PID, "guest process identity must survive restore")
		t.Logf("marathon: turn=%d pid=%d, save and restore", turn, guestPID)
		name := fmt.Sprintf("marathon-%d", turn)
		buildID := pause(name, current)
		current = resume(name+"-restored", buildID)
		sharingMarathonBacking(t, ctx, dir, name, current)
		// A guest-only cache eviction forces the filesystem oracle to re-read
		// the private upper. Never drop host-wide caches on this shared node.
		_, err = sharingExec(ctx, current, "sync; echo 3 > /proc/sys/vm/drop_caches")
		require.NoError(t, err)
		data, err := sharingExec(ctx, current, "cat /workspace/boundary.json")
		require.NoError(t, err)
		var restored map[string]any
		require.NoError(t, json.Unmarshal(data, &restored))
		require.Equal(t, float64(turn), restored["turn"])
		require.Equal(t, float64(guestPID), restored["pid"])
		require.Equal(t, boundary.SHA256, restored["sha256"])
		_, err = sharingExec(ctx, current, fmt.Sprintf("kill -0 %d && touch /workspace/continue-%d", guestPID, turn))
		require.NoError(t, err)
		checkpoints = append(checkpoints, map[string]any{"turn": turn, "build_id": buildID, "guest_pid": guestPID})
	}
	var result struct {
		Tools  int    `json:"tools"`
		Turns  int    `json:"turns"`
		SHA256 string `json:"sha256"`
		PID    int    `json:"pid"`
		Verify bool   `json:"verify"`
	}
	for {
		data := sharingMarathonPoll(t, ctx, current, "/workspace/result.json")
		if len(data) != 0 && json.Unmarshal(data, &result) == nil {
			break
		}
		sharingMarathonWait(t, ctx)
	}
	require.True(t, result.Verify)
	require.Equal(t, wantTools, result.Tools)
	require.Equal(t, len(trajectory.Turns), result.Turns)
	require.Equal(t, guestPID, result.PID)
	require.Equal(t, "aa44ebe9ce29d4563fabc1f42496ec16f6119b163b1a09d4b81df6f046dfcc62", result.SHA256, "same deterministic replay state as legacy")
	var events []byte
	for _, name := range []string{"events.jsonl", "result.json", "replay.log"} {
		data, err := sharingExec(ctx, current, "cat "+sharingQuote("/workspace/"+name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "guest-"+name), data, 0o600))
		if name == "events.jsonl" {
			events = data
		}
	}
	var calls []string
	var ready, resumed []int
	done, tools, starts := 0, 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(events), []byte("\n")) {
		var event struct {
			Event       string   `json:"event"`
			CallIDs     []string `json:"call_ids"`
			Turn        int      `json:"turn"`
			Tools       int      `json:"tools"`
			Turns       int      `json:"turns"`
			Verify      bool     `json:"verify"`
			SHA256      string   `json:"final_sha256"`
			TraceSHA256 string   `json:"trace_sha256"`
		}
		require.NoError(t, json.Unmarshal(line, &event))
		switch event.Event {
		case "start":
			starts++
			require.Equal(t, traceHash, event.TraceSHA256)
		case "tool_end":
			tools++
			calls = append(calls, event.CallIDs...)
		case "checkpoint_ready":
			ready = append(ready, event.Turn)
		case "checkpoint_resumed":
			require.True(t, event.Verify)
			resumed = append(resumed, event.Turn)
		case "replay_done":
			done++
			require.True(t, event.Verify)
			require.Equal(t, wantTools, event.Tools)
			require.Equal(t, len(trajectory.Turns), event.Turns)
			require.Equal(t, result.SHA256, event.SHA256)
		}
	}
	require.Equal(t, 1, starts)
	require.Equal(t, 1, done)
	require.Equal(t, wantTools, tools)
	require.Equal(t, wantCalls, calls, "all trace call IDs must occur once in original order")
	require.Equal(t, []int{10, 200, 700}, ready)
	require.Equal(t, ready, resumed)
	sharingReceipt(t, filepath.Join(dir, "marathon-summary.json"), map[string]any{
		"status": "passed", "evidence": "trace-driven synthetic memory replay; no original tool/LLM execution",
		"trace_name": trajectory.Name, "trace_sha256": traceHash, "turns": result.Turns, "tools": tools,
		"call_ids": len(calls), "verified_resumes": len(resumed), "checkpoints": checkpoints, "result": result,
		"pmem_dax": true, "shared_nbd_map_private_ram": true, "private_nbd_upper": true,
		"ram_export_mode": os.Getenv("E2B_SHARING_RAM_EXPORT_MODE"), "time_scale": 0.001,
		"elapsed_s": time.Since(started).Seconds(), "same_frozen_native_ram_oracle": true,
	})
	return current
}

func sharingMarathonUpload(t *testing.T, ctx context.Context, sbx *Sandbox, path string, data []byte) {
	t.Helper()
	_, err := sharingExec(ctx, sbx, "truncate -s 0 "+sharingQuote(path))
	require.NoError(t, err)
	for off := 0; off < len(data); off += 24 * 1024 {
		encoded := base64.StdEncoding.EncodeToString(data[off:min(off+24*1024, len(data))])
		_, err = sharingExec(ctx, sbx, "printf %s "+sharingQuote(encoded)+" | base64 -d >> "+sharingQuote(path))
		require.NoError(t, err)
	}
	actual, err := sharingExec(ctx, sbx, "sha256sum "+sharingQuote(path))
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(data)), strings.Fields(string(actual))[0])
}

func sharingMarathonPoll(t *testing.T, ctx context.Context, sbx *Sandbox, path string) []byte {
	t.Helper()
	command := "if test -s " + sharingQuote(path) + "; then cat " + sharingQuote(path) + "; else kill -0 $(cat /workspace/replay.pid) || { cat /workspace/replay.log >&2; exit 1; }; fi"
	data, err := sharingExec(ctx, sbx, command)
	require.NoError(t, err, "replay failed; guest replay.log is included in stderr")
	return data
}

func sharingMarathonWait(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(300 * time.Millisecond):
	}
}

func sharingMarathonBacking(t *testing.T, ctx context.Context, dir, name string, sbx *Sandbox) {
	t.Helper()
	ram, ok := sbx.Resources.memory.(*leasedMemory)
	require.True(t, ok, "trace must actually use the shared NBD RAM backend")
	pid, err := sbx.process.Pid()
	require.NoError(t, err)
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	require.NoError(t, err)
	var actual string
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[1] == "rw-p" && strings.Join(fields[5:], " ") == ram.lease.Path() {
			actual = line
			break
		}
	}
	require.NotEmpty(t, actual, "actual FC mapping must be MAP_PRIVATE over the leased NBD")
	provider, ok := sbx.rootfs.(*rootfs.PmemProvider)
	require.True(t, ok)
	binding := provider.PmemBinding()
	// Read and map a real application path, verifying DAX and the resulting
	// guest PFN in the separate PMEM GPA window, rather than mount flags alone.
	program := `import ctypes,json,mmap,os,struct
libc=ctypes.CDLL(None,use_errno=True);libc.mmap.restype=ctypes.c_void_p
path='/sbin/sharing-init';attrs={}
for name in [path,'/.e2b/lower'+path]:
 st=ctypes.create_string_buffer(256)
 assert libc.statx(-100,name.encode(),0,0x7ff,ctypes.byref(st))==0
 a=struct.unpack_from('<Q',st.raw,8)[0];mask=struct.unpack_from('<Q',st.raw,56)[0]
 assert a&0x200000 and mask&0x200000
 attrs[name]={'attributes':a,'mask':mask}
fd=os.open(path,os.O_RDONLY);ptr=libc.mmap(None,4096,mmap.PROT_READ,mmap.MAP_SHARED,fd,0)
assert ptr not in (None,ctypes.c_void_p(-1).value)
assert ctypes.string_at(ptr,1)==open(path,'rb').read(1)
with open('/proc/self/pagemap','rb',buffering=0) as f:
 f.seek(ptr//4096*8);entry=struct.unpack('<Q',f.read(8))[0]
assert entry>>63 and entry&((1<<55)-1)
print(json.dumps({'gpa':(entry&((1<<55)-1))*4096,'statx':attrs}))`
	data, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(program))
	require.NoError(t, err)
	var dax struct {
		GPA uint64 `json:"gpa"`
	}
	require.NoError(t, json.Unmarshal(data, &dax))
	require.GreaterOrEqual(t, dax.GPA, binding.GuestAddress)
	require.Less(t, dax.GPA, binding.GuestAddress+binding.Size)
	sharingReceipt(t, filepath.Join(dir, "marathon-"+name+"-backing.json"), map[string]any{
		"host_pid": pid, "nbd_path": ram.lease.Path(), "actual_ram_mapping": actual,
		"pmem": binding, "guest_dax": json.RawMessage(data),
	})
}
