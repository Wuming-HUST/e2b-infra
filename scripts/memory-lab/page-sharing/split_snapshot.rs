//! Test fixture tool: split contiguous RAM into real KVM slots without GPA holes.
use std::fs::File;
use std::io::Write;
use vmm::persist::MicrovmState;
use vmm::snapshot::Snapshot;
use vmm::vstate::memory::GuestMemoryRegionState;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    assert!(args.len() >= 4, "INPUT OUTPUT CUT_OFFSET [CUT_OFFSET ...]");
    let mut input = File::open(&args[1]).unwrap();
    let len = input.metadata().unwrap().len() as usize;
    let (mut state, version): (MicrovmState, _) = Snapshot::load(&mut input, len).unwrap();
    assert_eq!(state.vm_state.memory.regions.len(), 1);
    let original = &state.vm_state.memory.regions[0];
    let base = original.base_address;
    let size = original.size;
    let mut cuts: Vec<usize> = args[3..].iter().map(|s| s.parse().unwrap()).collect();
    cuts.push(size);
    let mut previous = 0;
    let mut regions = Vec::new();
    for cut in cuts {
        assert!(cut > previous && cut <= size && cut % 4096 == 0);
        regions.push(GuestMemoryRegionState {
            base_address: base + previous as u64,
            size: cut - previous,
        });
        previous = cut;
    }
    assert_eq!(previous, size);
    state.vm_state.memory.regions = regions;
    let mut output = File::create_new(&args[2]).unwrap();
    Snapshot::new(version).save(&mut output, &state).unwrap();
    output.flush().unwrap();
    output.sync_all().unwrap();
    println!("{:?}", state.vm_state.memory);
}
