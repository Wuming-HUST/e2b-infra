//go:build linux

package sandbox

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
)

//go:embed testdata/pmem_compat.py
var pmemCompatibilityGuest string

//go:embed testdata/pmem_overlay_baseline.py
var pmemOverlayBaselineGuest string

// Extends the physical-sharing test, including its same-frozen-state RAM oracle.
// Requires the explicit compatibility lower fixture; no live image is modified.
func TestPmemCompatibilityIntegration(t *testing.T) { //nolint:paralleltest
	t.Setenv("E2B_PMEM_COMPAT", "1")
	TestPmemSharingProofIntegration(t)
}

type pmemCompatibilityState struct {
	Epoch          int               `json:"epoch"`
	PID            int               `json:"pid"`
	ExecPID        int               `json:"exec_pid"`
	ExecGPA        uint64            `json:"exec_gpa"`
	BufferGPA      uint64            `json:"buffer_gpa"`
	BufferSHA      string            `json:"buffer_sha256"`
	OriginalSHA    string            `json:"original_sha256"`
	OpenGPAs       map[string]uint64 `json:"open_mmap_gpas"`
	DirectoryErrno *int              `json:"directory_rename_errno"`
}

func sharingCompatStart(t *testing.T, ctx context.Context, sbx *Sandbox) {
	t.Helper()
	baseline, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(pmemOverlayBaselineGuest))
	require.NoError(t, err)
	var reference map[string]any
	require.NoError(t, json.Unmarshal(baseline, &reference))
	require.Equal(t, true, reference["non_dax"])
	_, err = sharingExec(ctx, sbx, "printf %s "+sharingQuote(string(baseline))+" >/run/pmem-overlay-baseline.json")
	require.NoError(t, err)
	_, err = sharingExec(ctx, sbx, "python3 -c "+sharingQuote(pmemCompatibilityGuest)+" >/run/pmem-compat.log 2>&1 </dev/null &")
	require.NoError(t, err)
	_, err = sharingExec(ctx, sbx, "for i in $(seq 1 100); do test ! -f /run/pmem-compat.ready || exit 0; sleep .05; done; cat /run/pmem-compat.log; exit 1")
	require.NoError(t, err)
	sharingCompatState(t, ctx, sbx, "read", 0)
}

func sharingCompatCommand(t *testing.T, ctx context.Context, sbx *Sandbox, command map[string]any) []byte {
	t.Helper()
	payload, err := json.Marshal(command)
	require.NoError(t, err)
	program := "import socket; s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM); s.settimeout(20); s.connect('/run/pmem-compat.sock'); s.sendall(" + strconv.Quote(string(payload)) + ".encode()); print(s.makefile().readline().strip())"
	output, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(program))
	if err != nil || !json.Valid(output) {
		diagnostic, _ := sharingExec(ctx, sbx, "cat /run/pmem-compat.log")
		t.Logf("compat guest log: %s", diagnostic)
	}
	require.NoError(t, err)
	require.True(t, json.Valid(output), "compatibility guest must return a JSON receipt: %q", output)
	return output
}

func sharingCompatState(t *testing.T, ctx context.Context, sbx *Sandbox, command string, epoch int) pmemCompatibilityState {
	t.Helper()
	var state pmemCompatibilityState
	data := sharingCompatCommand(t, ctx, sbx, map[string]any{"op": command})
	require.NoError(t, json.Unmarshal(data, &state), string(data))
	require.Equal(t, epoch, state.Epoch)
	require.NotZero(t, state.PID)
	require.NotZero(t, state.ExecPID)
	require.NotZero(t, state.BufferGPA)
	require.GreaterOrEqual(t, state.BufferGPA, uint64(0x80000000))
	require.Less(t, state.BufferGPA, uint64(0x90000000))
	binding := sbx.rootfs.(*rootfs.PmemProvider).PmemBinding()
	for _, address := range append([]uint64{state.ExecGPA}, mapValues(state.OpenGPAs)...) {
		require.GreaterOrEqual(t, address, binding.GuestAddress)
		require.Less(t, address, binding.GuestAddress+binding.Size)
	}
	return state
}

func mapValues(m map[string]uint64) []uint64 {
	values := make([]uint64, 0, len(m))
	for _, value := range m {
		values = append(values, value)
	}
	return values
}

func sharingCompatPair(t *testing.T, ctx context.Context, dir, mode string, left, right *Sandbox) {
	t.Helper()
	// readinto must execute AFTER restoring each VM. A buffer filled before A
	// is allowed to share A's RAM snapshot until either VM writes to it.
	l, r := sharingCompatState(t, ctx, left, "read-buffer", 0), sharingCompatState(t, ctx, right, "read-buffer", 0)
	require.Equal(t, l.OriginalSHA, r.OriginalSHA)
	require.Equal(t, l.BufferSHA, r.BufferSHA)
	pages := func(sbx *Sandbox, state pmemCompatibilityState) (sharingProofPage, sharingProofPage) {
		p, _ := sharingPmemMapping(t, sbx)
		binding := sbx.rootfs.(*rootfs.PmemProvider).PmemBinding()
		executable := sharingProofHostPage(t, sbx, p["mapping_address"].(uint64)+state.ExecGPA-binding.GuestAddress)
		buffer := sharingProofHostPage(t, sbx, sharingProofRAMAddress(t, ctx, sbx, state.BufferGPA))
		return executable, buffer
	}
	le, lb := pages(left, l)
	re, rb := pages(right, r)
	require.Equal(t, le.PFN, re.PFN, "running executable lower text must share a physical page")
	require.NotEqual(t, lb.PFN, rb.PFN, "read() buffers must remain per-VM private")
	transfers := sharingCompatTransfers(t, ctx, left)
	changed := sharingCompatState(t, ctx, left, "mutate", 1)
	baseline, err := sharingExec(ctx, left, "cat /run/pmem-overlay-baseline.json")
	require.NoError(t, err)
	var reference struct {
		DirectoryErrno int `json:"directory_rename_errno"`
	}
	require.NoError(t, json.Unmarshal(baseline, &reference))
	require.NotNil(t, changed.DirectoryErrno)
	require.Equal(t, reference.DirectoryErrno, *changed.DirectoryErrno, "DAX and ordinary OverlayFS must agree on directory rename")
	unchanged := sharingCompatState(t, ctx, right, "read", 0)
	require.Equal(t, r, unchanged)
	sharingReceipt(t, filepath.Join(dir, mode+"-compat-pair.json"), map[string]any{
		"left_before": l, "right_before": r, "left_after": changed, "right_after": unchanged,
		"left_exec": le, "right_exec": re, "left_buffer": lb, "right_buffer": rb, "transfers": transfers,
		"non_dax_overlay_reference": json.RawMessage(baseline)})
}

func sharingCompatTransfers(t *testing.T, ctx context.Context, sbx *Sandbox) []any {
	t.Helper()
	var receipts []any
	for _, kind := range []string{"sendfile", "splice"} {
		var local map[string]any
		require.NoError(t, json.Unmarshal(sharingCompatCommand(t, ctx, sbx, map[string]any{"op": "transfer", "kind": kind}), &local))
		// The production firewall denies guest-initiated connections to the
		// link-local tap address. Initiate from the host and exercise guest TX
		// on the established connection, keeping the firewall intact.
		var ready struct {
			Port int `json:"port"`
		}
		require.NoError(t, json.Unmarshal(sharingCompatCommand(t, ctx, sbx, map[string]any{"op": "transfer-start", "kind": kind}), &ready))
		require.Greater(t, ready.Port, 0)
		connection, err := net.DialTimeout("tcp", net.JoinHostPort(sbx.Slot.HostIPString(), strconv.Itoa(ready.Port)), 10*time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = connection.Close() })
		require.NoError(t, connection.SetDeadline(time.Now().Add(20*time.Second)))
		data, err := io.ReadAll(io.LimitReader(connection, 8193))
		require.NoError(t, err)
		require.NoError(t, connection.Close())
		remote := sharingCompatCommand(t, ctx, sbx, map[string]any{"op": "transfer-finish"})
		want := make([]byte, 8192)
		for i := range want {
			want[i] = byte(i % 256)
		}
		require.Equal(t, want, data, fmt.Sprintf("%s DAX source through virtio TCP", kind))
		receipts = append(receipts, map[string]any{"loopback": local, "virtio_tcp": json.RawMessage(remote), "host_received_bytes": len(data), "host_content_verified": true})
	}
	return receipts
}

// A sacrificial restored VM attempts to bypass the read-only filesystem mount.
// The observer VM and the complete host lower hash must remain unchanged.
func sharingCompatAttack(t *testing.T, ctx context.Context, dir, name, kind string, attacker, observer *Sandbox) {
	t.Helper()
	before := sharingCompatState(t, ctx, observer, "read", 2)
	state := sharingCompatState(t, ctx, attacker, "read", 0)
	binding := attacker.rootfs.(*rootfs.PmemProvider).PmemBinding()
	hash, size := sharingHashFile(t, binding.PathOnHost)
	var program string
	if kind == "cpu" {
		program = fmt.Sprintf("import mmap,os; f=os.open('/dev/mem',os.O_RDWR|os.O_SYNC); m=mmap.mmap(f,4096,flags=mmap.MAP_SHARED,prot=mmap.PROT_READ|mmap.PROT_WRITE,offset=%d); m[0]=m[0]^0x20; raise RuntimeError('LOWER_WRITE_WAS_ACCEPTED')", state.ExecGPA/4096*4096)
	} else {
		program = "import os; f=os.open('/dev/pmem0',os.O_RDWR|os.O_SYNC); old=os.pread(f,4096,0); assert len(old)==4096; n=os.pwrite(f,bytes([old[0]^0x20])+old[1:],0); os.fsync(f); raise RuntimeError('LOWER_WRITE_WAS_ACCEPTED bytes='+str(n))"
	}
	attackCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, attackErr := sharingExec(attackCtx, attacker, "python3 -c "+sharingQuote(program))
	require.Error(t, attackErr, "raw lower mutation must never succeed")
	require.NotContains(t, attackErr.Error(), "LOWER_WRITE_WAS_ACCEPTED")
	require.NoError(t, ctx.Err())
	hashAfter, sizeAfter := sharingHashFile(t, binding.PathOnHost)
	require.Equal(t, hash, hashAfter)
	require.Equal(t, size, sizeAfter)
	after := sharingCompatState(t, ctx, observer, "read", 2)
	require.Equal(t, before, after)
	exited := false
	select {
	case <-attacker.process.Exit.Done():
		exited = true
	default:
	}
	if !exited {
		// A permission error or guest abort is also a controlled refusal. The
		// surviving attacker must remain usable, rather than silently hang.
		sharingCompatState(t, ctx, attacker, "read", 0)
		require.Contains(t, attackErr.Error(), "stderr=", "non-exiting attack must return a concrete guest error")
	}
	sharingReceipt(t, filepath.Join(dir, name+"-attack.json"), map[string]any{
		"kind": kind, "controlled_refusal": true, "fc_exited": exited, "error": attackErr.Error(), "output": string(output),
		"lower_sha256_before": hash, "lower_sha256_after": hashAfter, "lower_size": size,
		"observer_before": before, "observer_after": after})
}
