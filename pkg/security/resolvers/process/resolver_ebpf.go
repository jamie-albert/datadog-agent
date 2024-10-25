// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux

// Package process holds process related files
package process

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
	manager "github.com/DataDog/ebpf-manager"
	lib "github.com/cilium/ebpf"
	"github.com/hashicorp/golang-lru/v2/simplelru"
	"github.com/shirou/gopsutil/v3/process"
	"go.uber.org/atomic"
	"golang.org/x/sys/unix"

	"github.com/DataDog/datadog-agent/pkg/process/procutil"
	"github.com/DataDog/datadog-agent/pkg/security/metrics"
	"github.com/DataDog/datadog-agent/pkg/security/probe/config"
	"github.com/DataDog/datadog-agent/pkg/security/probe/managerhelper"
	"github.com/DataDog/datadog-agent/pkg/security/resolvers/cgroup"
	"github.com/DataDog/datadog-agent/pkg/security/resolvers/container"
	"github.com/DataDog/datadog-agent/pkg/security/resolvers/envvars"
	"github.com/DataDog/datadog-agent/pkg/security/resolvers/mount"
	spath "github.com/DataDog/datadog-agent/pkg/security/resolvers/path"
	"github.com/DataDog/datadog-agent/pkg/security/resolvers/usergroup"
	"github.com/DataDog/datadog-agent/pkg/security/secl/containerutils"
	"github.com/DataDog/datadog-agent/pkg/security/secl/model"
	"github.com/DataDog/datadog-agent/pkg/security/seclog"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
	stime "github.com/DataDog/datadog-agent/pkg/util/ktime"
)

const (
	Snapshotting = iota // Snapshotting describes the state where resolvers are being populated
	Snapshotted         // Snapshotted describes the state where resolvers are fully populated
)

const (
	procResolveMaxDepth              = 16
	maxParallelArgsEnvs              = 512 // == number of parallel starting processes
	argsEnvsValueCacheSize           = 8192
	numAllowedPIDsToResolvePerPeriod = 1
	procFallbackLimiterPeriod        = 30 * time.Second // proc fallback period by pid
)

// EBPFResolver resolved process context
type EBPFResolver struct {
	sync.RWMutex
	state *atomic.Int64

	manager      *manager.Manager
	config       *config.Config
	statsdClient statsd.ClientInterface
	scrubber     *procutil.DataScrubber

	containerResolver *container.Resolver
	mountResolver     mount.ResolverInterface
	cgroupResolver    *cgroup.Resolver
	userGroupResolver *usergroup.Resolver
	timeResolver      *stime.Resolver
	pathResolver      spath.ResolverInterface
	envVarsResolver   *envvars.Resolver

	execFileCacheMap *lib.Map
	procCacheMap     *lib.Map
	pidCacheMap      *lib.Map
	opts             ResolverOpts

	// stats
	cacheSize                 *atomic.Int64
	hitsStats                 map[string]*atomic.Int64
	missStats                 *atomic.Int64
	addedEntriesFromEvent     *atomic.Int64
	addedEntriesFromKernelMap *atomic.Int64
	addedEntriesFromProcFS    *atomic.Int64
	flushedEntries            *atomic.Int64
	pathErrStats              *atomic.Int64
	argsTruncated             *atomic.Int64
	argsSize                  *atomic.Int64
	envsTruncated             *atomic.Int64
	envsSize                  *atomic.Int64
	brokenLineage             *atomic.Int64
	inodeErrStats             *atomic.Int64

	entryCache    map[uint32]*model.ProcessCacheEntry
	argsEnvsCache *simplelru.LRU[uint64, *argsEnvsCacheEntry]

	processCacheEntryPool *Pool

	// limiters
	procFallbackLimiter *utils.Limiter[uint32]

	exitedQueue []uint32
}

// DequeueExited dequeue exited process
func (p *EBPFResolver) DequeueExited() {
	p.Lock()
	defer p.Unlock()

	delEntry := func(pid uint32, exitTime time.Time) {
		p.deleteEntry(pid, exitTime)
		p.flushedEntries.Inc()
	}

	now := time.Now()
	for _, pid := range p.exitedQueue {
		entry := p.entryCache[pid]
		if entry == nil {
			continue
		}

		if tm := entry.ExecTime; !tm.IsZero() && tm.Add(time.Minute).Before(now) {
			delEntry(pid, now)
		} else if tm := entry.ForkTime; !tm.IsZero() && tm.Add(time.Minute).Before(now) {
			delEntry(pid, now)
		} else if entry.ForkTime.IsZero() && entry.ExecTime.IsZero() {
			delEntry(pid, now)
		}
	}

	p.exitedQueue = p.exitedQueue[0:0]
}

// NewProcessCacheEntry returns a new process cache entry
func (p *EBPFResolver) NewProcessCacheEntry(pidContext model.PIDContext) *model.ProcessCacheEntry {
	entry := p.processCacheEntryPool.Get()
	entry.PIDContext = pidContext
	entry.Cookie = utils.NewCookie()
	return entry
}

// CountBrokenLineage increments the counter of broken lineage
func (p *EBPFResolver) CountBrokenLineage() {
	p.brokenLineage.Inc()
}

// SendStats sends process resolver metrics
func (p *EBPFResolver) SendStats() error {
	if err := p.statsdClient.Gauge(metrics.MetricProcessResolverCacheSize, p.getCacheSize(), []string{}, 1.0); err != nil {
		return fmt.Errorf("failed to send process_resolver cache_size metric: %w", err)
	}

	if err := p.statsdClient.Gauge(metrics.MetricProcessResolverReferenceCount, p.getEntryCacheSize(), []string{}, 1.0); err != nil {
		return fmt.Errorf("failed to send process_resolver reference_count metric: %w", err)
	}

	for _, resolutionType := range metrics.AllTypesTags {
		if count := p.hitsStats[resolutionType].Swap(0); count > 0 {
			if err := p.statsdClient.Count(metrics.MetricProcessResolverHits, count, []string{resolutionType}, 1.0); err != nil {
				return fmt.Errorf("failed to send process_resolver with `%s` metric: %w", resolutionType, err)
			}
		}
	}

	if count := p.missStats.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverMiss, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver misses metric: %w", err)
		}
	}

	if count := p.addedEntriesFromEvent.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverAdded, count, metrics.ProcessSourceEventTags, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver added entries metric: %w", err)
		}
	}

	if count := p.addedEntriesFromKernelMap.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverAdded, count, metrics.ProcessSourceKernelMapsTags, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver added entries from kernel map metric: %w", err)
		}
	}

	if count := p.addedEntriesFromProcFS.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverAdded, count, metrics.ProcessSourceProcTags, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver added entries from kernel map metric: %w", err)
		}
	}

	if count := p.flushedEntries.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverFlushed, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver flushed entries metric: %w", err)
		}
	}

	if count := p.pathErrStats.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverPathError, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver path error metric: %w", err)
		}
	}

	if count := p.argsTruncated.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverArgsTruncated, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send args truncated metric: %w", err)
		}
	}

	if count := p.argsSize.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverArgsSize, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send args size metric: %w", err)
		}
	}

	if count := p.envsTruncated.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverEnvsTruncated, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send envs truncated metric: %w", err)
		}
	}

	if count := p.envsSize.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessResolverEnvsSize, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send envs size metric: %w", err)
		}
	}

	if count := p.brokenLineage.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessEventBrokenLineage, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver broken lineage metric: %w", err)
		}
	}

	if count := p.inodeErrStats.Swap(0); count > 0 {
		if err := p.statsdClient.Count(metrics.MetricProcessInodeError, count, []string{}, 1.0); err != nil {
			return fmt.Errorf("failed to send process_resolver inode error metric: %w", err)
		}
	}

	return nil
}

type argsEnvsCacheEntry struct {
	values    []string
	truncated bool
}

var argsEnvsInterner = utils.NewLRUStringInterner(argsEnvsValueCacheSize)

func parseStringArray(data []byte) ([]string, bool) {
	truncated := false
	values, err := model.UnmarshalStringArray(data)
	if err != nil || len(data) == model.MaxArgEnvSize {
		if len(values) > 0 {
			values[len(values)-1] += "..."
		}
		truncated = true
	}

	argsEnvsInterner.DeduplicateSlice(values)
	return values, truncated
}

func newArgsEnvsCacheEntry(event *model.ArgsEnvsEvent) *argsEnvsCacheEntry {
	values, truncated := parseStringArray(event.ValuesRaw[:event.Size])
	return &argsEnvsCacheEntry{
		values:    values,
		truncated: truncated,
	}
}

func (e *argsEnvsCacheEntry) extend(event *model.ArgsEnvsEvent) {
	values, truncated := parseStringArray(event.ValuesRaw[:event.Size])
	if truncated {
		e.truncated = true
	}
	e.values = append(e.values, values...)
}

// UpdateArgsEnvs updates arguments or environment variables of the given id
func (p *EBPFResolver) UpdateArgsEnvs(event *model.ArgsEnvsEvent) {
	if list, found := p.argsEnvsCache.Get(event.ID); found {
		list.extend(event)
	} else {
		p.argsEnvsCache.Add(event.ID, newArgsEnvsCacheEntry(event))
	}
}

// AddForkEntry adds an entry to the local cache and returns the newly created entry
func (p *EBPFResolver) AddForkEntry(entry *model.ProcessCacheEntry, inode uint64) {
	if entry.Pid == 0 {
		return
	}

	p.Lock()
	defer p.Unlock()

	p.insertForkEntry(entry, inode, model.ProcessCacheEntryFromEvent)
}

// AddExecEntry adds an entry to the local cache and returns the newly created entry
func (p *EBPFResolver) AddExecEntry(entry *model.ProcessCacheEntry, inode uint64) {
	if entry.Pid == 0 {
		return
	}

	p.Lock()
	defer p.Unlock()

	p.insertExecEntry(entry, inode, model.ProcessCacheEntryFromEvent)
}

// enrichEventFromProc uses /proc to enrich a ProcessCacheEntry with additional metadata
func (p *EBPFResolver) enrichEventFromProc(entry *model.ProcessCacheEntry, proc *process.Process, filledProc *utils.FilledProcess) error {
	// the provided process is a kernel process if its virtual memory size is null
	if filledProc.MemInfo.VMS == 0 {
		return fmt.Errorf("cannot snapshot kernel threads")
	}
	pid := uint32(proc.Pid)

	// Get process filename and pre-fill the cache
	procExecPath := utils.ProcExePath(pid)
	pathnameStr, err := os.Readlink(procExecPath)
	if err != nil {
		return fmt.Errorf("snapshot failed for %d: couldn't readlink binary: %w", proc.Pid, err)
	}
	if pathnameStr == "/ (deleted)" {
		return fmt.Errorf("snapshot failed for %d: binary was deleted", proc.Pid)
	}

	// Get the file fields of the process binary
	info, err := p.retrieveExecFileFields(procExecPath)
	if err != nil {
		return fmt.Errorf("snapshot failed for %d: couldn't retrieve inode info: %w", proc.Pid, err)
	}

	// Retrieve the container ID of the process from /proc
	containerID, containerFlags, err := p.containerResolver.GetContainerContext(pid)
	if err != nil {
		return fmt.Errorf("snapshot failed for %d: couldn't parse container ID: %w", proc.Pid, err)
	}

	entry.FileEvent.FileFields = *info
	setPathname(&entry.FileEvent, pathnameStr)

	// force mount from procfs/snapshot
	entry.FileEvent.MountOrigin = model.MountOriginProcfs
	entry.FileEvent.MountSource = model.MountSourceSnapshot

	entry.Process.CGroup.CGroupFlags = containerFlags
	var fileStats unix.Statx_t

	taskPath := utils.CgroupTaskPath(pid, pid)
	if err := unix.Statx(unix.AT_FDCWD, taskPath, 0, unix.STATX_ALL, &fileStats); err == nil {
		entry.Process.CGroup.CGroupFile.MountID = uint32(fileStats.Mnt_id)
		entry.Process.CGroup.CGroupFile.Inode = fileStats.Ino
	} else {
		// Get the file fields of the cgroup file
		info, err := p.retrieveExecFileFields(taskPath)
		if err != nil {
			seclog.Debugf("snapshot failed for %d: couldn't retrieve inode info: %s", proc.Pid, err)
		} else {
			entry.Process.CGroup.CGroupFile.MountID = info.MountID
		}
	}

	if cgroupFileContent, err := os.ReadFile(taskPath); err == nil {
		lines := strings.Split(string(cgroupFileContent), "\n")
		for _, line := range lines {
			parts := strings.SplitN(line, ":", 3)

			// Skip potentially malformed lines
			if len(parts) != 3 {
				continue
			}

			entry.Process.CGroup.CGroupID = containerutils.CGroupID(parts[2])
			break
		}
	}

	if entry.FileEvent.IsFileless() {
		entry.FileEvent.Filesystem = model.TmpFS
	} else {
		// resolve container path with the MountEBPFResolver
		entry.FileEvent.Filesystem, err = p.mountResolver.ResolveFilesystem(entry.Process.FileEvent.MountID, entry.Process.FileEvent.Device, entry.Process.Pid, string(containerID))
		if err != nil {
			seclog.Debugf("snapshot failed for mount %d with pid %d : couldn't get the filesystem: %s", entry.Process.FileEvent.MountID, proc.Pid, err)
		}
	}

	entry.ExecTime = time.Unix(0, filledProc.CreateTime*int64(time.Millisecond))
	entry.ForkTime = entry.ExecTime
	entry.Comm = filledProc.Name
	entry.PPid = uint32(filledProc.Ppid)
	entry.TTYName = utils.PidTTY(uint32(filledProc.Pid))
	entry.ProcessContext.Pid = pid
	entry.ProcessContext.Tid = pid
	if len(filledProc.Uids) >= 4 {
		entry.Credentials.UID = uint32(filledProc.Uids[0])
		entry.Credentials.EUID = uint32(filledProc.Uids[1])
		entry.Credentials.FSUID = uint32(filledProc.Uids[3])
	}
	if len(filledProc.Gids) >= 4 {
		entry.Credentials.GID = uint32(filledProc.Gids[0])
		entry.Credentials.EGID = uint32(filledProc.Gids[1])
		entry.Credentials.FSGID = uint32(filledProc.Gids[3])
	}
	// fetch login_uid
	entry.Credentials.AUID, err = utils.GetLoginUID(uint32(proc.Pid))
	if err != nil {
		return fmt.Errorf("snapshot failed for %d: couldn't get login UID: %w", proc.Pid, err)
	}

	entry.Credentials.CapEffective, entry.Credentials.CapPermitted, err = utils.CapEffCapEprm(uint32(proc.Pid))
	if err != nil {
		return fmt.Errorf("snapshot failed for %d: couldn't parse kernel capabilities: %w", proc.Pid, err)
	}
	p.SetProcessUsersGroups(entry)

	// args and envs
	entry.ArgsEntry = &model.ArgsEntry{}
	if len(filledProc.Cmdline) > 0 {
		entry.ArgsEntry.Values = filledProc.Cmdline
	}

	entry.EnvsEntry = &model.EnvsEntry{}
	if envs, truncated, err := p.envVarsResolver.ResolveEnvVars(uint32(proc.Pid)); err == nil {
		entry.EnvsEntry.Values = envs
		entry.EnvsEntry.Truncated = truncated
	}

	// Heuristic to detect likely interpreter event
	// Cannot detect when a script if as follows:
	// perl <<__HERE__
	// #!/usr/bin/perl
	//
	// sleep 10;
	//
	// print "Hello from Perl\n";
	// __HERE__
	// Because the entry only has 1 argument (perl in this case). But can detect when a script is as follows:
	// cat << EOF > perlscript.pl
	// #!/usr/bin/perl
	//
	// sleep 15;
	//
	// print "Hello from Perl\n";
	//
	// EOF
	if values := entry.ArgsEntry.Values; len(values) > 1 {
		firstArg := values[0]
		lastArg := values[len(values)-1]
		// Example result: comm value: pyscript.py | args: [/usr/bin/python3 ./pyscript.py]
		if path.Base(lastArg) == entry.Comm && path.IsAbs(firstArg) {
			entry.LinuxBinprm.FileEvent = entry.FileEvent
		}
	}

	if !entry.HasInterpreter() {
		// mark it as resolved to avoid abnormal path later in the call flow
		entry.LinuxBinprm.FileEvent.SetPathnameStr("")
		entry.LinuxBinprm.FileEvent.SetBasenameStr("")
	}

	// add netns
	entry.NetNS, _ = utils.NetNSPathFromPid(pid).GetProcessNetworkNamespace()

	if p.config.NetworkEnabled {
		// snapshot pid routes in kernel space
		_, _ = proc.OpenFiles()
	}

	return nil
}

// retrieveExecFileFields fetches inode metadata from kernel space
func (p *EBPFResolver) retrieveExecFileFields(procExecPath string) (*model.FileFields, error) {
	fi, err := os.Stat(procExecPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot failed for `%s`: couldn't stat binary: %w", procExecPath, err)
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("snapshot failed for `%s`: couldn't stat binary", procExecPath)
	}
	inode := stat.Ino

	inodeb := make([]byte, 8)
	binary.NativeEndian.PutUint64(inodeb, inode)

	data, err := p.execFileCacheMap.LookupBytes(inodeb)
	if err != nil {
		return nil, fmt.Errorf("unable to get filename for inode `%d`: %v", inode, err)
	}

	var fileFields model.FileFields
	if _, err := fileFields.UnmarshalBinary(data); err != nil {
		return nil, fmt.Errorf("unable to unmarshal entry for inode `%d`", inode)
	}

	if fileFields.Inode == 0 {
		return nil, errors.New("not found")
	}

	return &fileFields, nil
}

func (p *EBPFResolver) insertEntry(entry, prev *model.ProcessCacheEntry, source uint64) {
	entry.Source = source
	p.entryCache[entry.Pid] = entry
	entry.Retain()

	if prev != nil {
		prev.Release()
	}

	if p.cgroupResolver != nil && entry.ContainerID != "" {
		// add the new PID in the right cgroup_resolver bucket
		p.cgroupResolver.AddPID(entry)
	}

	switch source {
	case model.ProcessCacheEntryFromEvent:
		p.addedEntriesFromEvent.Inc()
	case model.ProcessCacheEntryFromKernelMap:
		p.addedEntriesFromKernelMap.Inc()
	case model.ProcessCacheEntryFromProcFS:
		p.addedEntriesFromProcFS.Inc()
	}

	p.cacheSize.Inc()
}

func (p *EBPFResolver) insertForkEntry(entry *model.ProcessCacheEntry, inode uint64, source uint64) {
	if entry.Pid == 0 {
		return
	}

	prev := p.entryCache[entry.Pid]
	if prev != nil {
		// this shouldn't happen but it is better to exit the prev and let the new one replace it
		prev.Exit(entry.ForkTime)
	}

	if entry.Pid != 1 {
		parent := p.entryCache[entry.PPid]
		if entry.PPid >= 1 && inode != 0 && (parent == nil || parent.FileEvent.Inode != inode) {
			if candidate := p.resolve(entry.PPid, entry.PPid, inode, true); candidate != nil {
				parent = candidate
			} else {
				entry.IsParentMissing = true
				p.inodeErrStats.Inc()
			}
		}

		if parent != nil {
			parent.Fork(entry)
		} else {
			entry.IsParentMissing = true
		}
	}

	p.insertEntry(entry, prev, source)
}

func (p *EBPFResolver) insertExecEntry(entry *model.ProcessCacheEntry, inode uint64, source uint64) {
	if entry.Pid == 0 {
		return
	}

	prev := p.entryCache[entry.Pid]
	if prev != nil {
		if inode != 0 && prev.FileEvent.Inode != inode {
			entry.IsParentMissing = true
			p.inodeErrStats.Inc()
		}

		// check exec bomb
		if prev.Equals(entry) {
			prev.ApplyExecTimeOf(entry)
			return
		}

		prev.Exec(entry)
	} else {
		entry.IsParentMissing = true
	}

	p.insertEntry(entry, prev, source)
}

func (p *EBPFResolver) deleteEntry(pid uint32, exitTime time.Time) {
	// Start by updating the exit timestamp of the pid cache entry
	entry, ok := p.entryCache[pid]
	if !ok {
		return
	}

	if p.cgroupResolver != nil {
		p.cgroupResolver.DelPIDWithID(string(entry.ContainerID), entry.Pid)
	}

	entry.Exit(exitTime)
	delete(p.entryCache, entry.Pid)
	entry.Release()
}

// DeleteEntry tries to delete an entry in the process cache
func (p *EBPFResolver) DeleteEntry(pid uint32, exitTime time.Time) {
	p.Lock()
	defer p.Unlock()

	p.deleteEntry(pid, exitTime)
}

// Resolve returns the cache entry for the given pid
func (p *EBPFResolver) Resolve(pid, tid uint32, inode uint64, useProcFS bool) *model.ProcessCacheEntry {
	if pid == 0 {
		return nil
	}

	p.Lock()
	defer p.Unlock()

	return p.resolve(pid, tid, inode, useProcFS)
}

func (p *EBPFResolver) resolve(pid, tid uint32, inode uint64, useProcFS bool) *model.ProcessCacheEntry {
	if entry := p.resolveFromCache(pid, tid, inode); entry != nil {
		p.hitsStats[metrics.CacheTag].Inc()
		return entry
	}

	if p.state.Load() != Snapshotted {
		return nil
	}

	// fallback to the kernel maps directly, the perf event may be delayed / may have been lost
	if entry := p.resolveFromKernelMaps(pid, tid, inode); entry != nil {
		p.hitsStats[metrics.KernelMapsTag].Inc()
		return entry
	}

	if !useProcFS {
		p.missStats.Inc()
		return nil
	}

	if p.procFallbackLimiter.Allow(pid) {
		// fallback to /proc, the in-kernel LRU may have deleted the entry
		if entry := p.resolveFromProcfs(pid, procResolveMaxDepth); entry != nil {
			p.hitsStats[metrics.ProcFSTag].Inc()
			return entry
		}
	}

	p.missStats.Inc()
	return nil
}

func (p *EBPFResolver) resolveFileFieldsPath(e *model.FileFields, pce *model.ProcessCacheEntry, ctrCtx *model.ContainerContext) (string, string, model.MountSource, model.MountOrigin, error) {
	var (
		pathnameStr, mountPath string
		source                 model.MountSource
		origin                 model.MountOrigin
		err                    error
		maxDepthRetry          = 3
	)

	for maxDepthRetry > 0 {
		pathnameStr, mountPath, source, origin, err = p.pathResolver.ResolveFileFieldsPath(e, &pce.PIDContext, ctrCtx)
		if err == nil {
			return pathnameStr, mountPath, source, origin, nil
		}
		parent, exists := p.entryCache[pce.PPid]
		if !exists {
			break
		}

		pce = parent
		maxDepthRetry--
	}

	return pathnameStr, mountPath, source, origin, err
}

// SetProcessPath resolves process file path
func (p *EBPFResolver) SetProcessPath(fileEvent *model.FileEvent, pce *model.ProcessCacheEntry, ctrCtx *model.ContainerContext) (string, error) {
	onError := func(pathnameStr string, err error) (string, error) {
		fileEvent.SetPathnameStr("")
		fileEvent.SetBasenameStr("")

		p.pathErrStats.Inc()

		return pathnameStr, err
	}

	if fileEvent.Inode == 0 {
		return onError("", &model.ErrInvalidKeyPath{Inode: fileEvent.Inode, MountID: fileEvent.MountID})
	}

	pathnameStr, mountPath, source, origin, err := p.resolveFileFieldsPath(&fileEvent.FileFields, pce, ctrCtx)
	if err != nil {
		return onError(pathnameStr, err)
	}
	setPathname(fileEvent, pathnameStr)
	fileEvent.MountPath = mountPath
	fileEvent.MountSource = source
	fileEvent.MountOrigin = origin

	return fileEvent.PathnameStr, nil
}

// SetProcessSymlink resolves process file symlink path
func (p *EBPFResolver) SetProcessSymlink(entry *model.ProcessCacheEntry) {
	// TODO: busybox workaround only for now
	if IsBusybox(entry.FileEvent.PathnameStr) {
		arg0, _ := GetProcessArgv0(&entry.Process)
		base := path.Base(arg0)

		entry.SymlinkPathnameStr[0] = "/bin/" + base
		entry.SymlinkPathnameStr[1] = "/usr/bin/" + base

		entry.SymlinkBasenameStr = base
	}
}

// SetProcessFilesystem resolves process file system
func (p *EBPFResolver) SetProcessFilesystem(entry *model.ProcessCacheEntry) (string, error) {
	if entry.FileEvent.MountID != 0 {
		fs, err := p.mountResolver.ResolveFilesystem(entry.FileEvent.MountID, entry.FileEvent.Device, entry.Pid, string(entry.ContainerID))
		if err != nil {
			return "", err
		}
		entry.FileEvent.Filesystem = fs
	}

	return entry.FileEvent.Filesystem, nil
}

// ApplyBootTime realign timestamp from the boot time
func (p *EBPFResolver) ApplyBootTime(entry *model.ProcessCacheEntry) {
	entry.ExecTime = p.timeResolver.ApplyBootTime(entry.ExecTime)
	entry.ForkTime = p.timeResolver.ApplyBootTime(entry.ForkTime)
	entry.ExitTime = p.timeResolver.ApplyBootTime(entry.ExitTime)
}

// ResolveFromCache resolves cache entry from the cache
func (p *EBPFResolver) ResolveFromCache(pid, tid uint32, inode uint64) *model.ProcessCacheEntry {
	p.Lock()
	defer p.Unlock()
	return p.resolveFromCache(pid, tid, inode)
}

func (p *EBPFResolver) resolveFromCache(pid, tid uint32, inode uint64) *model.ProcessCacheEntry {
	entry, exists := p.entryCache[pid]
	if !exists {
		return nil
	}

	// Compare inode to ensure that the cache is up-to-date.
	// Be sure to compare with the file inode and not the pidcontext which can be empty
	// if the entry originates from procfs.
	if inode != 0 && inode != entry.Process.FileEvent.Inode {
		return nil
	}

	// make to update the tid with the that triggers the resolution
	entry.Tid = tid

	return entry
}

// ResolveNewProcessCacheEntry resolves the context fields of a new process cache entry parsed from kernel data
func (p *EBPFResolver) ResolveNewProcessCacheEntry(entry *model.ProcessCacheEntry, ctrCtx *model.ContainerContext) error {
	if _, err := p.SetProcessPath(&entry.FileEvent, entry, ctrCtx); err != nil {
		return &spath.ErrPathResolution{Err: fmt.Errorf("failed to resolve exec path: %w", err)}
	}

	if entry.HasInterpreter() {
		if _, err := p.SetProcessPath(&entry.LinuxBinprm.FileEvent, entry, ctrCtx); err != nil {
			return &spath.ErrPathResolution{Err: fmt.Errorf("failed to resolve interpreter path: %w", err)}
		}
	} else {
		// mark it as resolved to avoid abnormal path later in the call flow
		entry.LinuxBinprm.FileEvent.SetPathnameStr("")
		entry.LinuxBinprm.FileEvent.SetBasenameStr("")
	}

	p.SetProcessArgs(entry)
	p.SetProcessEnvs(entry)
	p.SetProcessTTY(entry)
	p.SetProcessUsersGroups(entry)
	p.ApplyBootTime(entry)
	p.SetProcessSymlink(entry)

	_, err := p.SetProcessFilesystem(entry)

	return err
}

// ResolveFromKernelMaps resolves the entry from the kernel maps
func (p *EBPFResolver) ResolveFromKernelMaps(pid, tid uint32, inode uint64) *model.ProcessCacheEntry {
	p.Lock()
	defer p.Unlock()
	return p.resolveFromKernelMaps(pid, tid, inode)
}

func (p *EBPFResolver) resolveFromKernelMaps(pid, tid uint32, inode uint64) *model.ProcessCacheEntry {
	if pid == 0 {
		return nil
	}

	pidb := make([]byte, 4)
	binary.NativeEndian.PutUint32(pidb, pid)

	pidCache, err := p.pidCacheMap.LookupBytes(pidb)
	if err != nil {
		// LookupBytes doesn't return an error if the key is not found thus it is a critical error
		seclog.Errorf("kernel map lookup error: %v", err)
	}
	if pidCache == nil {
		return nil
	}

	// first 4 bytes are the actual cookie
	procCache, err := p.procCacheMap.LookupBytes(pidCache[0:model.SizeOfCookie])
	if err != nil {
		// LookupBytes doesn't return an error if the key is not found thus it is a critical error
		seclog.Errorf("kernel map lookup error: %v", err)
	}
	if procCache == nil {
		return nil
	}

	entry := p.NewProcessCacheEntry(model.PIDContext{Pid: pid, Tid: tid, ExecInode: inode})

	var ctrCtx model.ContainerContext
	read, err := ctrCtx.UnmarshalBinary(procCache)
	if err != nil {
		return nil
	}

	var cgroupCtx model.CGroupContext
	cgroupRead, err := cgroupCtx.UnmarshalBinary(procCache)
	if err != nil {
		return nil
	}

	if _, err := entry.UnmarshalProcEntryBinary(procCache[read+cgroupRead:]); err != nil {
		return nil
	}

	// check that the cache entry correspond to the event
	if entry.FileEvent.Inode != 0 && entry.FileEvent.Inode != entry.ExecInode {
		return nil
	}

	if _, err := entry.UnmarshalPidCacheBinary(pidCache); err != nil {
		return nil
	}

	// resolve paths and other context fields
	if err = p.ResolveNewProcessCacheEntry(entry, &ctrCtx); err != nil {
		return nil
	}

	// If we fall back to the kernel maps for a process in a container that was already running when the agent
	// started, the kernel space container ID will be empty even though the process is inside a container. Since there
	// is no insurance that the parent of this process is still running, we can't use our user space cache to check if
	// the parent is in a container. In other words, we have to fall back to /proc to query the container ID of the
	// process.
	if entry.ContainerID == "" {
		containerID, containerFlags, err := p.containerResolver.GetContainerContext(pid)
		if err == nil {
			entry.CGroup.CGroupFlags = containerFlags
			entry.CGroup.CGroupID = containerutils.GetCgroupFromContainer(containerID, containerFlags)
		}
	}

	if entry.ExecTime.IsZero() {
		p.insertForkEntry(entry, entry.FileEvent.Inode, model.ProcessCacheEntryFromKernelMap)
	} else {
		p.insertExecEntry(entry, 0, model.ProcessCacheEntryFromKernelMap)
	}

	return entry
}

// ResolveFromProcfs resolves the entry from procfs
func (p *EBPFResolver) ResolveFromProcfs(pid uint32) *model.ProcessCacheEntry {
	p.Lock()
	defer p.Unlock()
	return p.resolveFromProcfs(pid, procResolveMaxDepth)
}

func (p *EBPFResolver) resolveFromProcfs(pid uint32, maxDepth int) *model.ProcessCacheEntry {
	if maxDepth < 1 {
		seclog.Tracef("max depth reached during procfs resolution: %d", pid)
		return nil
	}

	if pid == 0 {
		seclog.Tracef("no pid: %d", pid)
		return nil
	}

	var ppid uint32
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		seclog.Tracef("unable to find pid: %d", pid)
		return nil
	}

	filledProc, err := utils.GetFilledProcess(proc)
	if err != nil {
		seclog.Tracef("unable to get a filled process for pid %d: %d", pid, err)
		return nil
	}

	// ignore kthreads
	if IsKThread(uint32(filledProc.Ppid), uint32(filledProc.Pid)) {
		return nil
	}

	entry, inserted := p.syncCache(proc, filledProc, model.ProcessCacheEntryFromProcFS)
	if entry != nil {
		// consider kworker processes with 0 as ppid
		entry.IsKworker = filledProc.Ppid == 0 && filledProc.Pid != 1

		ppid = uint32(filledProc.Ppid)

		parent := p.resolveFromProcfs(ppid, maxDepth-1)
		if inserted && parent != nil {
			if parent.Equals(entry) {
				entry.SetParentOfForkChild(parent)
			} else {
				entry.SetAncestor(parent)
			}
		}
	}

	return entry
}

// SetProcessArgs set arguments to cache entry
func (p *EBPFResolver) SetProcessArgs(pce *model.ProcessCacheEntry) {
	if entry, found := p.argsEnvsCache.Get(pce.ArgsID); found {
		if pce.ArgsTruncated {
			p.argsTruncated.Inc()
		}

		p.argsSize.Add(int64(len(entry.values)))

		pce.ArgsEntry = &model.ArgsEntry{
			Values:    entry.values,
			Truncated: entry.truncated,
		}

		// no need to keep it in LRU now as attached to a process
		p.argsEnvsCache.Remove(pce.ArgsID)
	}
}

// GetProcessArgvScrubbed returns the scrubbed args of the event as an array
func (p *EBPFResolver) GetProcessArgvScrubbed(pr *model.Process) ([]string, bool) {
	if pr.ArgsEntry == nil || pr.ScrubbedArgvResolved {
		return pr.Argv, pr.ArgsTruncated
	}

	if p.scrubber != nil && len(pr.ArgsEntry.Values) > 0 {
		// replace with the scrubbed version
		argv, _ := p.scrubber.ScrubCommand(pr.ArgsEntry.Values[1:])
		pr.ArgsEntry.Values = []string{pr.ArgsEntry.Values[0]}
		pr.ArgsEntry.Values = append(pr.ArgsEntry.Values, argv...)
	}
	pr.ScrubbedArgvResolved = true

	return GetProcessArgv(pr)
}

// SetProcessEnvs set envs to cache entry
func (p *EBPFResolver) SetProcessEnvs(pce *model.ProcessCacheEntry) {
	if entry, found := p.argsEnvsCache.Get(pce.EnvsID); found {
		if pce.EnvsTruncated {
			p.envsTruncated.Inc()
		}

		p.envsSize.Add(int64(len(entry.values)))

		pce.EnvsEntry = &model.EnvsEntry{
			Values:    entry.values,
			Truncated: entry.truncated,
		}

		// no need to keep it in LRU now as attached to a process
		p.argsEnvsCache.Remove(pce.EnvsID)
	}
}

// GetProcessEnvs returns the envs of the event
func (p *EBPFResolver) GetProcessEnvs(pr *model.Process) ([]string, bool) {
	if pr.EnvsEntry == nil {
		return pr.Envs, pr.EnvsTruncated
	}

	keys, truncated := pr.EnvsEntry.FilterEnvs(p.opts.envsWithValue)
	pr.Envs = keys
	pr.EnvsTruncated = pr.EnvsTruncated || truncated
	return pr.Envs, pr.EnvsTruncated
}

// GetProcessEnvp returns the unscrubbed envs of the event with their values. Use with caution.
func (p *EBPFResolver) GetProcessEnvp(pr *model.Process) ([]string, bool) {
	if pr.EnvsEntry == nil {
		return pr.Envp, pr.EnvsTruncated
	}

	pr.Envp = pr.EnvsEntry.Values
	pr.EnvsTruncated = pr.EnvsTruncated || pr.EnvsEntry.Truncated
	return pr.Envp, pr.EnvsTruncated
}

// SetProcessTTY resolves TTY and cache the result
func (p *EBPFResolver) SetProcessTTY(pce *model.ProcessCacheEntry) string {
	if pce.TTYName == "" && p.opts.ttyFallbackEnabled {
		tty := utils.PidTTY(pce.Pid)
		pce.TTYName = tty
	}
	return pce.TTYName
}

// SetProcessUsersGroups resolves and set users and groups
func (p *EBPFResolver) SetProcessUsersGroups(pce *model.ProcessCacheEntry) {
	pce.User, _ = p.userGroupResolver.ResolveUser(int(pce.Credentials.UID), string(pce.ContainerID))
	pce.EUser, _ = p.userGroupResolver.ResolveUser(int(pce.Credentials.EUID), string(pce.ContainerID))
	pce.FSUser, _ = p.userGroupResolver.ResolveUser(int(pce.Credentials.FSUID), string(pce.ContainerID))

	pce.Group, _ = p.userGroupResolver.ResolveGroup(int(pce.Credentials.GID), string(pce.ContainerID))
	pce.EGroup, _ = p.userGroupResolver.ResolveGroup(int(pce.Credentials.EGID), string(pce.ContainerID))
	pce.FSGroup, _ = p.userGroupResolver.ResolveGroup(int(pce.Credentials.FSGID), string(pce.ContainerID))
}

// Get returns the cache entry for a specified pid
func (p *EBPFResolver) Get(pid uint32) *model.ProcessCacheEntry {
	p.RLock()
	defer p.RUnlock()
	return p.entryCache[pid]
}

// UpdateUID updates the credentials of the provided pid
func (p *EBPFResolver) UpdateUID(pid uint32, e *model.Event) {
	if e.ProcessContext.Pid != e.ProcessContext.Tid {
		return
	}

	p.Lock()
	defer p.Unlock()
	entry := p.entryCache[pid]
	if entry != nil {
		entry.Credentials.UID = e.SetUID.UID
		entry.Credentials.User = e.FieldHandlers.ResolveSetuidUser(e, &e.SetUID)
		entry.Credentials.EUID = e.SetUID.EUID
		entry.Credentials.EUser = e.FieldHandlers.ResolveSetuidEUser(e, &e.SetUID)
		entry.Credentials.FSUID = e.SetUID.FSUID
		entry.Credentials.FSUser = e.FieldHandlers.ResolveSetuidFSUser(e, &e.SetUID)
	}
}

// UpdateGID updates the credentials of the provided pid
func (p *EBPFResolver) UpdateGID(pid uint32, e *model.Event) {
	if e.ProcessContext.Pid != e.ProcessContext.Tid {
		return
	}

	p.Lock()
	defer p.Unlock()
	entry := p.entryCache[pid]
	if entry != nil {
		entry.Credentials.GID = e.SetGID.GID
		entry.Credentials.Group = e.FieldHandlers.ResolveSetgidGroup(e, &e.SetGID)
		entry.Credentials.EGID = e.SetGID.EGID
		entry.Credentials.EGroup = e.FieldHandlers.ResolveSetgidEGroup(e, &e.SetGID)
		entry.Credentials.FSGID = e.SetGID.FSGID
		entry.Credentials.FSGroup = e.FieldHandlers.ResolveSetgidFSGroup(e, &e.SetGID)
	}
}

// UpdateCapset updates the credentials of the provided pid
func (p *EBPFResolver) UpdateCapset(pid uint32, e *model.Event) {
	if e.ProcessContext.Pid != e.ProcessContext.Tid {
		return
	}

	p.Lock()
	defer p.Unlock()
	entry := p.entryCache[pid]
	if entry != nil {
		entry.Credentials.CapEffective = e.Capset.CapEffective
		entry.Credentials.CapPermitted = e.Capset.CapPermitted
	}
}

// UpdateLoginUID updates the AUID of the provided pid
func (p *EBPFResolver) UpdateLoginUID(pid uint32, e *model.Event) {
	if e.ProcessContext.Pid != e.ProcessContext.Tid {
		return
	}

	p.Lock()
	defer p.Unlock()
	entry := p.entryCache[pid]
	if entry != nil {
		entry.Credentials.AUID = e.LoginUIDWrite.AUID
	}
}

// UpdateAWSSecurityCredentials updates the list of AWS Security Credentials
func (p *EBPFResolver) UpdateAWSSecurityCredentials(pid uint32, e *model.Event) {
	if len(e.IMDS.AWS.SecurityCredentials.AccessKeyID) == 0 {
		return
	}

	p.Lock()
	defer p.Unlock()

	entry := p.entryCache[pid]
	if entry != nil {
		// check if this key is already in cache
		for _, key := range entry.AWSSecurityCredentials {
			if key.AccessKeyID == e.IMDS.AWS.SecurityCredentials.AccessKeyID {
				return
			}
		}
		entry.AWSSecurityCredentials = append(entry.AWSSecurityCredentials, e.IMDS.AWS.SecurityCredentials)
	}
}

// FetchAWSSecurityCredentials returns the list of AWS Security Credentials valid at the time of the event, and prunes
// expired entries
func (p *EBPFResolver) FetchAWSSecurityCredentials(e *model.Event) []model.AWSSecurityCredentials {
	p.Lock()
	defer p.Unlock()

	entry := p.entryCache[e.ProcessContext.Pid]
	if entry != nil {
		// check if we should delete
		var toDelete []int
		for id, key := range entry.AWSSecurityCredentials {
			if key.Expiration.Before(e.ResolveEventTime()) {
				toDelete = append([]int{id}, toDelete...)
			}
		}

		// delete expired entries
		for _, id := range toDelete {
			entry.AWSSecurityCredentials = append(entry.AWSSecurityCredentials[0:id], entry.AWSSecurityCredentials[id+1:]...)
		}

		return entry.AWSSecurityCredentials
	}
	return nil
}

// Start starts the resolver
func (p *EBPFResolver) Start(ctx context.Context) error {
	var err error
	if p.execFileCacheMap, err = managerhelper.Map(p.manager, "exec_file_cache"); err != nil {
		return err
	}

	if p.procCacheMap, err = managerhelper.Map(p.manager, "proc_cache"); err != nil {
		return err
	}

	if p.pidCacheMap, err = managerhelper.Map(p.manager, "pid_cache"); err != nil {
		return err
	}

	go p.cacheFlush(ctx)

	return nil
}

func (p *EBPFResolver) cacheFlush(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			procPids, err := process.Pids()
			if err != nil {
				continue
			}
			procPidsMap := make(map[uint32]bool)
			for _, pid := range procPids {
				procPidsMap[uint32(pid)] = true
			}

			p.Lock()
			for pid := range p.entryCache {
				if _, exists := procPidsMap[pid]; !exists {
					if entry := p.entryCache[pid]; entry != nil {
						p.exitedQueue = append(p.exitedQueue, pid)
					}
				}
			}
			p.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// SyncCache snapshots /proc for the provided pid. This method returns true if it updated the process cache.
func (p *EBPFResolver) SyncCache(proc *process.Process) bool {
	// Only a R lock is necessary to check if the entry exists, but if it exists, we'll update it, so a RW lock is
	// required.
	p.Lock()
	defer p.Unlock()

	filledProc, err := utils.GetFilledProcess(proc)
	if err != nil {
		seclog.Tracef("unable to get a filled process for %d: %v", proc.Pid, err)
		return false
	}

	_, ret := p.syncCache(proc, filledProc, model.ProcessCacheEntryFromSnapshot)
	return ret
}

func (p *EBPFResolver) setAncestor(pce *model.ProcessCacheEntry) {
	parent := p.entryCache[pce.PPid]
	if parent != nil {
		pce.SetAncestor(parent)
	}
}

// syncCache snapshots /proc for the provided pid. This method returns true if it updated the process cache.
func (p *EBPFResolver) syncCache(proc *process.Process, filledProc *utils.FilledProcess, source uint64) (*model.ProcessCacheEntry, bool) {
	pid := uint32(proc.Pid)

	// Check if an entry is already in cache for the given pid.
	entry := p.entryCache[pid]
	if entry != nil {
		p.setAncestor(entry)

		return entry, false
	}

	entry = p.NewProcessCacheEntry(model.PIDContext{Pid: pid, Tid: pid})
	entry.IsThread = true

	// update the cache entry
	if err := p.enrichEventFromProc(entry, proc, filledProc); err != nil {
		entry.Release()

		seclog.Trace(err)
		return nil, false
	}

	parent := p.entryCache[entry.PPid]
	if parent != nil {
		if parent.Equals(entry) {
			entry.SetParentOfForkChild(parent)
		} else {
			entry.SetAncestor(parent)
		}
	}

	p.insertEntry(entry, p.entryCache[pid], source)

	bootTime := p.timeResolver.GetBootTime()

	// insert new entry in kernel maps
	procCacheEntryB := make([]byte, 248)
	_, err := entry.Process.MarshalProcCache(procCacheEntryB, bootTime)
	if err != nil {
		seclog.Errorf("couldn't marshal proc_cache entry: %s", err)
	} else {
		if err = p.procCacheMap.Put(entry.Cookie, procCacheEntryB); err != nil {
			seclog.Errorf("couldn't push proc_cache entry to kernel space: %s", err)
		}
	}
	pidCacheEntryB := make([]byte, 88)
	_, err = entry.Process.MarshalPidCache(pidCacheEntryB, bootTime)
	if err != nil {
		seclog.Errorf("couldn't marshal pid_cache entry: %s", err)
	} else {
		if err = p.pidCacheMap.Put(pid, pidCacheEntryB); err != nil {
			seclog.Errorf("couldn't push pid_cache entry to kernel space: %s", err)
		}
	}

	seclog.Tracef("New process cache entry added: %s %s %d/%d", entry.Comm, entry.FileEvent.PathnameStr, pid, entry.FileEvent.Inode)

	return entry, true
}

// ToJSON return a json version of the cache
func (p *EBPFResolver) ToJSON() ([]byte, error) {
	dump := struct {
		Entries []json.RawMessage
	}{}

	p.Walk(func(entry *model.ProcessCacheEntry) {
		e := struct {
			PID             uint32
			PPID            uint32
			Path            string
			Inode           uint64
			MountID         uint32
			Source          string
			ExecInode       uint64
			IsThread        bool
			IsParentMissing bool
		}{
			PID:             entry.Pid,
			PPID:            entry.PPid,
			Path:            entry.FileEvent.PathnameStr,
			Inode:           entry.FileEvent.Inode,
			MountID:         entry.FileEvent.MountID,
			Source:          model.ProcessSourceToString(entry.Source),
			ExecInode:       entry.ExecInode,
			IsThread:        entry.IsThread,
			IsParentMissing: entry.IsParentMissing,
		}

		d, err := json.Marshal(e)
		if err == nil {
			dump.Entries = append(dump.Entries, d)
		}
	})

	return json.Marshal(dump)
}

func (p *EBPFResolver) toDot(writer io.Writer, entry *model.ProcessCacheEntry, already map[string]bool, withArgs bool) {
	for entry != nil {
		label := fmt.Sprintf("%s:%d", entry.Comm, entry.Pid)
		if _, exists := already[label]; !exists {
			if !entry.ExitTime.IsZero() {
				label = "[" + label + "]"
			}

			if withArgs {
				argv, _ := p.GetProcessArgvScrubbed(&entry.Process)
				fmt.Fprintf(writer, `"%d:%s" [label="%s", comment="%s"];`, entry.Pid, entry.Comm, label, strings.Join(argv, " "))
			} else {
				fmt.Fprintf(writer, `"%d:%s" [label="%s"];`, entry.Pid, entry.Comm, label)
			}
			fmt.Fprintln(writer)

			already[label] = true
		}

		if entry.Ancestor != nil {
			relation := fmt.Sprintf(`"%d:%s" -> "%d:%s";`, entry.Ancestor.Pid, entry.Ancestor.Comm, entry.Pid, entry.Comm)
			if _, exists := already[relation]; !exists {
				fmt.Fprintln(writer, relation)

				already[relation] = true
			}
		}

		entry = entry.Ancestor
	}
}

// ToDot create a temp file and dump the cache
func (p *EBPFResolver) ToDot(withArgs bool) (string, error) {
	dump, err := os.CreateTemp("/tmp", "process-cache-dump-")
	if err != nil {
		return "", err
	}

	defer dump.Close()

	if err := os.Chmod(dump.Name(), 0400); err != nil {
		return "", err
	}

	p.RLock()
	defer p.RUnlock()

	fmt.Fprintf(dump, "digraph ProcessTree {\n")

	already := make(map[string]bool)
	for _, entry := range p.entryCache {
		p.toDot(dump, entry, already, withArgs)
	}

	fmt.Fprintf(dump, `}`)

	if err = dump.Close(); err != nil {
		return "", fmt.Errorf("could not close file [%s]: %w", dump.Name(), err)
	}
	return dump.Name(), nil
}

// getCacheSize returns the cache size of the process resolver
func (p *EBPFResolver) getCacheSize() float64 {
	p.RLock()
	defer p.RUnlock()
	return float64(len(p.entryCache))
}

// getEntryCacheSize returns the cache size of the process resolver
func (p *EBPFResolver) getEntryCacheSize() float64 {
	return float64(p.cacheSize.Load())
}

// SetState sets the process resolver state
func (p *EBPFResolver) SetState(state int64) {
	p.state.Store(state)
}

// Walk iterates through the entire tree and call the provided callback on each entry
func (p *EBPFResolver) Walk(callback func(entry *model.ProcessCacheEntry)) {
	p.RLock()
	defer p.RUnlock()

	for _, entry := range p.entryCache {
		callback(entry)
	}
}

// NewEBPFResolver returns a new process resolver
func NewEBPFResolver(manager *manager.Manager, config *config.Config, statsdClient statsd.ClientInterface,
	scrubber *procutil.DataScrubber, containerResolver *container.Resolver, mountResolver mount.ResolverInterface,
	cgroupResolver *cgroup.Resolver, userGroupResolver *usergroup.Resolver, timeResolver *stime.Resolver,
	pathResolver spath.ResolverInterface, opts *ResolverOpts) (*EBPFResolver, error) {
	argsEnvsCache, err := simplelru.NewLRU[uint64, *argsEnvsCacheEntry](maxParallelArgsEnvs, nil)
	if err != nil {
		return nil, err
	}

	p := &EBPFResolver{
		manager:                   manager,
		config:                    config,
		statsdClient:              statsdClient,
		scrubber:                  scrubber,
		entryCache:                make(map[uint32]*model.ProcessCacheEntry),
		opts:                      *opts,
		argsEnvsCache:             argsEnvsCache,
		state:                     atomic.NewInt64(Snapshotting),
		hitsStats:                 map[string]*atomic.Int64{},
		cacheSize:                 atomic.NewInt64(0),
		missStats:                 atomic.NewInt64(0),
		addedEntriesFromEvent:     atomic.NewInt64(0),
		addedEntriesFromKernelMap: atomic.NewInt64(0),
		addedEntriesFromProcFS:    atomic.NewInt64(0),
		flushedEntries:            atomic.NewInt64(0),
		pathErrStats:              atomic.NewInt64(0),
		argsTruncated:             atomic.NewInt64(0),
		argsSize:                  atomic.NewInt64(0),
		envsTruncated:             atomic.NewInt64(0),
		envsSize:                  atomic.NewInt64(0),
		brokenLineage:             atomic.NewInt64(0),
		inodeErrStats:             atomic.NewInt64(0),
		containerResolver:         containerResolver,
		mountResolver:             mountResolver,
		cgroupResolver:            cgroupResolver,
		userGroupResolver:         userGroupResolver,
		timeResolver:              timeResolver,
		pathResolver:              pathResolver,
		envVarsResolver:           envvars.NewEnvVarsResolver(config),
	}
	for _, t := range metrics.AllTypesTags {
		p.hitsStats[t] = atomic.NewInt64(0)
	}
	p.processCacheEntryPool = NewProcessCacheEntryPool(func() { p.cacheSize.Dec() })

	// Create rate limiter that allows for 128 pids
	limiter, err := utils.NewLimiter[uint32](128, numAllowedPIDsToResolvePerPeriod, procFallbackLimiterPeriod)
	if err != nil {
		return nil, err
	}
	p.procFallbackLimiter = limiter

	return p, nil
}
