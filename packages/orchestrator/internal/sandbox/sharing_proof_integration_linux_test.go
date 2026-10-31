//go:build linux

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
)

// This test deliberately requires real visible PFNs, rather than accepting a
// common path or aggregate RSS as a physical-sharing oracle. Inputs and root
// requirements are the same as TestPmemFactoryIntegration.
func TestPmemSharingProofIntegration(t *testing.T) { //nolint:paralleltest
	t.Setenv("E2B_SHARING_PROOF", "1")
	TestPmemFactoryIntegration(t)
}

// The server, its private marker pages, open lower FD and DAX mmap survive each
// snapshot in guest RAM. Commands only arrive through the restored envd API.
// Host code never writes or prefaults the measured guest pages.
const sharingProofGuest = `import ctypes,hashlib,json,mmap,os,socket,struct
P=4096
assert os.sysconf('SC_PAGE_SIZE')==P
libc=ctypes.CDLL(None,use_errno=True)
libc.mmap.restype=ctypes.c_void_p
libc.mmap.argtypes=[ctypes.c_void_p,ctypes.c_size_t,ctypes.c_int,ctypes.c_int,ctypes.c_int,ctypes.c_long]
ram=mmap.mmap(-1,3*P,flags=mmap.MAP_PRIVATE|mmap.MAP_ANONYMOUS,prot=mmap.PROT_READ|mmap.PROT_WRITE)
for i in range(3):ram[i*P:(i+1)*P]=bytes([0x31+i])*P
addr=ctypes.addressof(ctypes.c_char.from_buffer(ram))
assert libc.mlock(ctypes.c_void_p(addr),ctypes.c_size_t(3*P))==0
path='/sbin/sharing-init'
original=open(path,'rb').read()
assert 0<len(original)<=P
def map_file():
 fd=os.open(path,os.O_RDONLY)
 ptr=libc.mmap(None,P,mmap.PROT_READ,mmap.MAP_SHARED,fd,0)
 assert ptr not in (None,ctypes.c_void_p(-1).value)
 return fd,ptr
fd,ptr=map_file()
assert ctypes.string_at(ptr,len(original))==original
epoch=0
def gpa(address):
 with open('/proc/self/pagemap','rb',buffering=0) as f:
  f.seek(address//P*8);entry=struct.unpack('<Q',f.read(8))[0]
 assert entry>>63 and entry&((1<<55)-1)
 return (entry&((1<<55)-1))*P+address%P
def sha(data):return hashlib.sha256(data).hexdigest()
def state():
 current=open(path,'rb').read()
 newfd,newptr=map_file()
 assert ctypes.string_at(newptr,min(P,len(current)))==current
 view_gpa=gpa(newptr)
 assert libc.munmap(ctypes.c_void_p(newptr),ctypes.c_size_t(P))==0
 os.close(newfd)
 # statx on the final overlay path and the actual lower inode are recorded
 # independently of the stronger mmap-GPA-in-PMEM check on the host.
 attrs={}
 for name in [path,'/.e2b/lower'+path]:
  st=ctypes.create_string_buffer(256)
  assert libc.statx(-100,name.encode(),0,0x7ff,ctypes.byref(st))==0
  attrs[name]={'attributes':struct.unpack_from('<Q',st.raw,8)[0],'mask':struct.unpack_from('<Q',st.raw,56)[0]}
 return {'epoch':epoch,'ram_gpas':[gpa(addr+i*P) for i in range(3)],
  'ram_sha256':[sha(ram[i*P:(i+1)*P]) for i in range(3)],
  'file_gpa':gpa(ptr),'current_file_gpa':view_gpa,'original_file_sha256':sha(original),
  'mapped_file_sha256':sha(ctypes.string_at(ptr,len(original))),
  'current_file_sha256':sha(current),'lower_file_sha256':sha(open('/.e2b/lower'+path,'rb').read()),
  'expected_current_sha256':sha(original if epoch==0 else bytes([original[0]^0x20])+original[1:]+(b'proof-C' if epoch==2 else b'')),
  'statx':attrs,'pid':os.getpid(),'file_path':path}
s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
s.bind('/run/e2b-sharing-proof.sock');s.listen(4)
open('/run/e2b-sharing-proof.ready','w').close()
while True:
 c,_=s.accept()
 try:
  cmd=c.recv(128).decode().strip()
  if cmd=='mutate':
   assert epoch==0
   ram[P:2*P]=bytes([0xa5])*P;ram[2*P:3*P]=bytes(P)
   with open(path,'r+b') as f:f.write(bytes([original[0]^0x20]));f.flush();os.fsync(f.fileno())
   epoch=1
  elif cmd=='mutate2':
   assert epoch==1
   ram[:P]=bytes([0x7e])*P
   with open(path,'ab') as f:f.write(b'proof-C');f.flush();os.fsync(f.fileno())
   epoch=2
  else:assert cmd=='read'
  c.sendall(json.dumps(state()).encode()+b'\n')
 finally:c.close()
`

type sharingProofGuestState struct {
	Epoch                 int            `json:"epoch"`
	RAMGPAs               []uint64       `json:"ram_gpas"`
	RAMSHA256             []string       `json:"ram_sha256"`
	FileGPA               uint64         `json:"file_gpa"`
	CurrentFileGPA        uint64         `json:"current_file_gpa"`
	OriginalFileSHA256    string         `json:"original_file_sha256"`
	MappedFileSHA256      string         `json:"mapped_file_sha256"`
	CurrentFileSHA256     string         `json:"current_file_sha256"`
	LowerFileSHA256       string         `json:"lower_file_sha256"`
	ExpectedCurrentSHA256 string         `json:"expected_current_sha256"`
	Statx                 map[string]any `json:"statx"`
	PID                   int            `json:"pid"`
	FilePath              string         `json:"file_path"`
}

func sharingProofStart(t *testing.T, ctx context.Context, sbx *Sandbox) {
	t.Helper()
	_, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(sharingProofGuest)+" >/run/e2b-sharing-proof.log 2>&1 </dev/null &")
	require.NoError(t, err)
	_, err = sharingExec(ctx, sbx, "for i in $(seq 1 100); do test ! -f /run/e2b-sharing-proof.ready || exit 0; sleep .05; done; cat /run/e2b-sharing-proof.log; exit 1")
	require.NoError(t, err)
	sharingProofState(t, ctx, sbx, 0)
}

func sharingProofCommand(t *testing.T, ctx context.Context, sbx *Sandbox, command string) sharingProofGuestState {
	t.Helper()
	program := "import socket; s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM); s.settimeout(10); s.connect('/run/e2b-sharing-proof.sock'); s.sendall(" + strconv.Quote(command+"\n") + ".encode()); print(s.makefile().readline().strip())"
	output, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(program))
	require.NoError(t, err)
	var state sharingProofGuestState
	require.NoError(t, json.Unmarshal(output, &state), string(output))
	return state
}

func sharingProofState(t *testing.T, ctx context.Context, sbx *Sandbox, epoch int) sharingProofGuestState {
	t.Helper()
	s := sharingProofCommand(t, ctx, sbx, "read")
	require.Equal(t, epoch, s.Epoch)
	values := []byte{0x31, 0x32, 0x33}
	if epoch >= 1 {
		values[1], values[2] = 0xa5, 0
	}
	if epoch == 2 {
		values[0] = 0x7e
	}
	require.Len(t, s.RAMGPAs, 3)
	require.Len(t, s.RAMSHA256, 3)
	for i, value := range values {
		page := make([]byte, 4096)
		for j := range page {
			page[j] = value
		}
		hash := sha256.Sum256(page)
		require.Equal(t, hex.EncodeToString(hash[:]), s.RAMSHA256[i])
	}
	require.Equal(t, s.OriginalFileSHA256, s.MappedFileSHA256, "open lower mmap must survive copy-up and restore")
	require.Equal(t, s.OriginalFileSHA256, s.LowerFileSHA256)
	require.Equal(t, s.ExpectedCurrentSHA256, s.CurrentFileSHA256)
	provider, ok := sbx.rootfs.(*rootfs.PmemProvider)
	require.True(t, ok)
	b := provider.PmemBinding()
	require.GreaterOrEqual(t, s.FileGPA, b.GuestAddress+provider.PmemDataOffset(), "application mmap must point directly into PMEM")
	require.Less(t, s.FileGPA, b.GuestAddress+b.Size)
	if epoch == 0 {
		require.Equal(t, s.FileGPA, s.CurrentFileGPA)
	} else {
		require.NotEqual(t, s.OriginalFileSHA256, s.CurrentFileSHA256)
		require.GreaterOrEqual(t, s.CurrentFileGPA, uint64(0x80000000))
		require.Less(t, s.CurrentFileGPA, uint64(0x90000000), "copy-up file pages must be private ordinary guest RAM")
	}
	return s
}

type sharingProofPage struct {
	PID         int    `json:"pid"`
	HostAddress uint64 `json:"host_address"`
	Entry       uint64 `json:"entry"`
	PFN         uint64 `json:"pfn"`
	FilePage    bool   `json:"file_page"`
	Mapping     string `json:"mapping"`
}

func sharingProofHostPage(t *testing.T, sbx *Sandbox, address uint64) sharingProofPage {
	t.Helper()
	pid, err := sbx.process.Pid()
	require.NoError(t, err)
	f, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid))
	require.NoError(t, err)
	defer f.Close()
	var entry [8]byte
	_, err = f.ReadAt(entry[:], int64(address/4096*8))
	require.NoError(t, err)
	v := binary.LittleEndian.Uint64(entry[:])
	require.NotZero(t, v&(1<<63), "measured page must be present")
	pfn := v & ((1 << 55) - 1)
	require.NotZero(t, pfn, "visible host PFNs are mandatory; do not silently accept path reuse")
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	require.NoError(t, err)
	var mapping string
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		start, end, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		a, err := strconv.ParseUint(start, 16, 64)
		require.NoError(t, err)
		z, err := strconv.ParseUint(end, 16, 64)
		require.NoError(t, err)
		if address >= a && address < z {
			mapping = line
			break
		}
	}
	require.NotEmpty(t, mapping)
	return sharingProofPage{PID: pid, HostAddress: address, Entry: v, PFN: pfn, FilePage: v&(1<<61) != 0, Mapping: mapping}
}

func sharingProofRAMAddress(t *testing.T, ctx context.Context, sbx *Sandbox, gpa uint64) uint64 {
	t.Helper()
	// This target fixture is ARM64, exactly one 256 MiB RAM region at 2 GiB.
	// Check the actual mapping before translating; never apply this shortcut
	// to large/multi-region or x86 guests.
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sbx.files.SandboxFirecrackerSocketPath())
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/memory/mappings", nil)
	require.NoError(t, err)
	res, err := (&http.Client{Transport: transport}).Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	var response struct {
		Mappings []struct {
			Host   uint64 `json:"base_host_virt_addr"`
			Offset uint64 `json:"offset"`
			Size   uint64 `json:"size"`
		} `json:"mappings"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&response))
	require.Len(t, response.Mappings, 1)
	m := response.Mappings[0]
	require.Zero(t, m.Offset)
	require.Equal(t, uint64(256<<20), m.Size)
	require.GreaterOrEqual(t, gpa, uint64(0x80000000))
	require.Less(t, gpa, uint64(0x80000000)+m.Size)
	return m.Host + gpa - 0x80000000
}

func sharingProofPages(t *testing.T, ctx context.Context, sbx *Sandbox, state sharingProofGuestState) ([]sharingProofPage, sharingProofPage) {
	t.Helper()
	ram := make([]sharingProofPage, 3)
	for i, gpa := range state.RAMGPAs {
		ram[i] = sharingProofHostPage(t, sbx, sharingProofRAMAddress(t, ctx, sbx, gpa))
		require.Equal(t, "rw-p", strings.Fields(ram[i].Mapping)[1])
	}
	p, _ := sharingPmemMapping(t, sbx)
	b := sbx.rootfs.(*rootfs.PmemProvider).PmemBinding()
	file := sharingProofHostPage(t, sbx, p["mapping_address"].(uint64)+state.FileGPA-b.GuestAddress)
	require.Equal(t, "r--s", strings.Fields(file.Mapping)[1])
	return ram, file
}

func sharingProofPair(t *testing.T, ctx context.Context, dir, mode string, left, right *Sandbox) {
	t.Helper()
	ls, rs := sharingProofState(t, ctx, left, 0), sharingProofState(t, ctx, right, 0)
	require.Equal(t, ls.RAMGPAs, rs.RAMGPAs)
	require.Equal(t, ls.FileGPA, rs.FileGPA)
	var before, after []any
	for sample := 0; sample < 3; sample++ {
		lp, lf := sharingProofPages(t, ctx, left, ls)
		rp, rf := sharingProofPages(t, ctx, right, rs)
		for i := range lp {
			require.Equal(t, lp[i].PFN, rp[i].PFN, "clean nonzero RAM page %d must be physically shared", i)
			require.True(t, lp[i].FilePage && rp[i].FilePage)
		}
		require.Equal(t, lf.PFN, rf.PFN, "application DAX mmap must share the same physical file page")
		before = append(before, map[string]any{"left_ram": lp, "right_ram": rp, "left_rootfs": lf, "right_rootfs": rf})
		time.Sleep(20 * time.Millisecond)
	}
	sharingProofCommand(t, ctx, left, "mutate")
	ls, rs = sharingProofState(t, ctx, left, 1), sharingProofState(t, ctx, right, 0)
	for sample := 0; sample < 3; sample++ {
		lp, lf := sharingProofPages(t, ctx, left, ls)
		rp, rf := sharingProofPages(t, ctx, right, rs)
		require.Equal(t, lp[0].PFN, rp[0].PFN, "untouched marker stays shared")
		for _, i := range []int{1, 2} {
			require.NotEqual(t, lp[i].PFN, rp[i].PFN, "write and zero must COW independently")
			require.False(t, lp[i].FilePage)
			require.True(t, rp[i].FilePage)
		}
		require.Equal(t, lf.PFN, rf.PFN, "the already-open lower mmap remains shared after copy-up")
		after = append(after, map[string]any{"left_ram": lp, "right_ram": rp, "left_rootfs": lf, "right_rootfs": rf})
		time.Sleep(20 * time.Millisecond)
	}
	sharingReceipt(t, filepath.Join(dir, mode+"-physical-sharing-proof.json"), map[string]any{
		"before": before, "after": after, "left_after": ls, "right_after": rs,
		"guest_driven_faults_and_writes": true, "nonzero_markers": true, "samples_per_phase": 3})
}

func sharingProofControl(t *testing.T, ctx context.Context, dir string, shared, control *Sandbox) {
	t.Helper()
	ls, rs := sharingProofState(t, ctx, shared, 0), sharingProofState(t, ctx, control, 0)
	lp, lf := sharingProofPages(t, ctx, shared, ls)
	rp, rf := sharingProofPages(t, ctx, control, rs)
	require.Equal(t, ls.RAMSHA256, rs.RAMSHA256)
	require.Equal(t, ls.CurrentFileSHA256, rs.CurrentFileSHA256)
	for i := range lp {
		require.NotEqual(t, lp[i].PFN, rp[i].PFN, "identical bytes on a different RAM backing must not be attributed to shared NBD")
	}
	lower := shared.rootfs.(*rootfs.PmemProvider).PmemBinding()
	other := control.rootfs.(*rootfs.PmemProvider).PmemBinding()
	require.Equal(t, lower.ContentID, other.ContentID)
	a, err := os.Stat(lower.PathOnHost)
	require.NoError(t, err)
	b, err := os.Stat(other.PathOnHost)
	require.NoError(t, err)
	sa, sb := a.Sys().(*syscall.Stat_t), b.Sys().(*syscall.Stat_t)
	require.False(t, sa.Dev == sb.Dev && sa.Ino == sb.Ino)
	require.NotEqual(t, lf.PFN, rf.PFN)
	sharingReceipt(t, filepath.Join(dir, "different-backing-control.json"), map[string]any{
		"same_bytes": true, "same_lower_digest": lower.ContentID,
		"left_inode": sa.Ino, "right_inode": sb.Ino,
		"left_ram": lp, "right_ram": rp, "left_rootfs": lf, "right_rootfs": rf})
}

func sharingProofUnchangedSources(t *testing.T, ctx context.Context, dir, mode string, sbx *Sandbox, cache *template.Cache, aID string) {
	t.Helper()
	tmpl, err := cache.GetTemplate(ctx, aID, false, false)
	require.NoError(t, err)
	source, err := tmpl.Memfile(ctx)
	require.NoError(t, err)
	oracle, size := sharingHashFile(t, filepath.Join(dir, "A-native.ram"))
	digest := sha256.New()
	buffer := make([]byte, 1<<20)
	for off := int64(0); off < size; off += int64(len(buffer)) {
		chunk := buffer[:min(int64(len(buffer)), size-off)]
		n, err := source.ReadAt(ctx, chunk, off)
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
		_, _ = digest.Write(chunk)
	}
	ramHash := hex.EncodeToString(digest.Sum(nil))
	require.Equal(t, oracle, ramHash, "all private writes must leave template A unchanged")
	binding := sbx.rootfs.(*rootfs.PmemProvider).PmemBinding()
	lowerHash, lowerSize := sharingHashFile(t, binding.PathOnHost)
	require.Equal(t, binding.ContentID, lowerHash)
	require.Equal(t, binding.Size, uint64(lowerSize))
	inputHash, _ := sharingHashFile(t, os.Getenv("E2B_PMEM_LOWER"))
	require.Equal(t, binding.ContentID, inputHash)
	sharingReceipt(t, filepath.Join(dir, mode+"-unchanged-sources.json"), map[string]any{
		"ram_template": aID, "ram_sha256": ramHash, "ram_size": size,
		"rootfs_sha256": lowerHash, "rootfs_size": lowerSize, "input_rootfs_sha256": inputHash})
}
