package hostd

import (
	"sync"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/fc"
)

// VM is one microVM known to this hostd: its lifecycle state and the
// Firecracker client driving its process. Callers mutating State or
// issuing Firecracker calls that must appear atomic to concurrent
// requests (pause/resume/snapshot/load/cutover) hold mu for the
// duration.
type VM struct {
	mu sync.Mutex

	ID              string
	State           api.VMState
	Tap             api.TapConfig
	TrackDirtyPages bool
	Dir             string // /snapshots/{id}, this VM's default artifact directory
	SockPath        string
	LogPath         string
	FC              *fc.Client
}

// Info snapshots the VM's current identity fields into the wire
// type. Callers must hold vm.mu (or otherwise know no concurrent
// state transition is in flight) for a consistent read of State.
func (vm *VM) Info() api.VMInfo {
	return api.VMInfo{ID: vm.ID, State: vm.State, PID: vm.FC.PID()}
}

// registry is the set of VMs this hostd knows about, keyed by id.
type registry struct {
	mu  sync.RWMutex
	vms map[string]*VM
}

func newRegistry() *registry {
	return &registry{vms: make(map[string]*VM)}
}

func (r *registry) get(id string) (*VM, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	vm, ok := r.vms[id]
	return vm, ok
}

func (r *registry) add(vm *VM) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.vms[vm.ID]; exists {
		return false
	}
	r.vms[vm.ID] = vm
	return true
}

func (r *registry) remove(id string) (*VM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vm, ok := r.vms[id]
	if ok {
		delete(r.vms, id)
	}
	return vm, ok
}

func (r *registry) list() []*VM {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*VM, 0, len(r.vms))
	for _, vm := range r.vms {
		out = append(out, vm)
	}
	return out
}
