package calcium

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	"github.com/projecteru2/core/log"

	enginetypes "github.com/projecteru2/core/engine/types"
	resourcetypes "github.com/projecteru2/core/resources/types"
	"github.com/projecteru2/core/strategy"
	"github.com/projecteru2/core/types"
	"github.com/projecteru2/core/utils"
)

// PodResource show pod resource usage
func (c *Calcium) PodResource(ctx context.Context, podname string) (*types.PodResource, error) {
	logger := log.WithField("Calcium", "PodResource").WithField("podname", podname)
	nodes, err := c.ListPodNodes(ctx, podname, nil, true)
	if err != nil {
		return nil, logger.Err(err)
	}
	r := &types.PodResource{
		Name:          podname,
		NodesResource: []*types.NodeResource{},
	}
	for _, node := range nodes {
		nodeResource, err := c.doGetNodeResource(ctx, node.Name, false)
		if err != nil {
			return nil, logger.Err(err)
		}
		r.NodesResource = append(r.NodesResource, nodeResource)
	}
	return r, nil
}

// NodeResource check node's workload and resource
func (c *Calcium) NodeResource(ctx context.Context, nodename string, fix bool) (*types.NodeResource, error) {
	logger := log.WithField("Calcium", "NodeResource").WithField("nodename", nodename).WithField("fix", fix)
	if nodename == "" {
		return nil, logger.Err(types.ErrEmptyNodeName)
	}

	nr, err := c.doGetNodeResource(ctx, nodename, fix)
	if err != nil {
		return nil, logger.Err(err)
	}
	for _, workload := range nr.Workloads {
		if _, err := workload.Inspect(ctx); err != nil { // 用于探测节点上容器是否存在
			nr.Diffs = append(nr.Diffs, fmt.Sprintf("workload %s inspect failed %v \n", workload.ID, err))
			continue
		}
	}
	return nr, logger.Err(err)
}

func (c *Calcium) doGetNodeResource(ctx context.Context, nodename string, fix bool) (*types.NodeResource, error) {
	var nr *types.NodeResource
	return nr, c.withNodeLocked(ctx, nodename, func(ctx context.Context, node *types.Node) error {
		workloads, err := c.ListNodeWorkloads(ctx, node.Name, nil)
		if err != nil {
			return err
		}
		nr = &types.NodeResource{
			Name: node.Name, CPU: node.CPU, MemCap: node.MemCap, StorageCap: node.StorageCap,
			Workloads: workloads, Diffs: []string{},
		}

		cpus := 0.0
		memory := int64(0)
		storage := int64(0)
		cpumap := types.CPUMap{}
		for _, workload := range workloads {
			cpus = utils.Round(cpus + workload.CPUQuotaRequest)
			memory += workload.MemoryRequest
			storage += workload.StorageRequest
			cpumap.Add(workload.CPU)
		}
		nr.CPUPercent = cpus / float64(len(node.InitCPU))
		nr.MemoryPercent = float64(memory) / float64(node.InitMemCap)
		nr.NUMAMemoryPercent = map[string]float64{}
		nr.VolumePercent = float64(node.VolumeUsed) / float64(node.InitVolume.Total())
		for nodeID, nmemory := range node.NUMAMemory {
			if initMemory, ok := node.InitNUMAMemory[nodeID]; ok {
				nr.NUMAMemoryPercent[nodeID] = float64(nmemory) / float64(initMemory)
			}
		}
		if cpus != node.CPUUsed {
			nr.Diffs = append(nr.Diffs, fmt.Sprintf("cpus used: %f diff: %f", node.CPUUsed, cpus))
		}
		node.CPU.Add(cpumap)
		for i, v := range node.CPU {
			if node.InitCPU[i] != v {
				nr.Diffs = append(nr.Diffs, fmt.Sprintf("cpu %s diff %d", i, node.InitCPU[i]-v))
			}
		}

		if memory+node.MemCap != node.InitMemCap {
			nr.Diffs = append(nr.Diffs, fmt.Sprintf("memory used: %d, diff %d", node.MemCap, node.InitMemCap-(memory+node.MemCap)))
		}

		nr.StoragePercent = 0
		if node.InitStorageCap != 0 {
			nr.StoragePercent = float64(storage) / float64(node.InitStorageCap)
			if storage+node.StorageCap != node.InitStorageCap {
				nr.Diffs = append(nr.Diffs, fmt.Sprintf("storage used: %d, diff %d", node.StorageCap, node.InitStorageCap-(storage+node.StorageCap)))
			}
		}

		if err := node.Engine.ResourceValidate(ctx, cpus, cpumap, memory, storage); err != nil {
			nr.Diffs = append(nr.Diffs, err.Error())
		}

		if fix {
			if err := c.doFixDiffResource(ctx, node, cpus, memory, storage); err != nil {
				log.Warnf(ctx, "[doGetNodeResource] fix node resource failed %v", err)
			}
		}

		return nil
	})
}

func (c *Calcium) doFixDiffResource(ctx context.Context, node *types.Node, cpus float64, memory, storage int64) error {
	var n *types.Node
	var err error
	return utils.Txn(ctx,
		func(ctx context.Context) error {
			if n, err = c.GetNode(ctx, node.Name); err != nil {
				return err
			}
			n.CPUUsed = cpus
			for i, v := range node.CPU {
				n.CPU[i] += node.InitCPU[i] - v
			}
			n.MemCap += node.InitMemCap - (memory + node.MemCap)
			n.StorageCap += node.InitStorageCap - (storage + node.StorageCap)
			return nil
		},
		func(ctx context.Context) error {
			return errors.WithStack(c.store.UpdateNodes(ctx, n))
		},
		nil,
		c.config.GlobalTimeout,
	)
}

func (c *Calcium) doAllocResource(ctx context.Context, nodeMap map[string]*types.Node, opts *types.DeployOptions) ([]resourcetypes.ResourcePlans, map[string]int, error) {
	total, plans, strategyInfos, err := c.doCalculateCapacity(ctx, nodeMap, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := c.store.MakeDeployStatus(ctx, opts, strategyInfos); err != nil {
		return nil, nil, errors.WithStack(err)
	}
	deployMap, err := strategy.Deploy(ctx, opts, strategyInfos, total)
	if err != nil {
		return nil, nil, err
	}
	log.Infof(ctx, "[Calium.doAllocResource] deployMap: %+v", deployMap)
	return plans, deployMap, nil
}

// called on changes of resource binding, such as cpu binding
// as an internal api, remap doesn't lock node, the responsibility of that should be taken on by caller
func (c *Calcium) remapResource(ctx context.Context, node *types.Node) (ch <-chan enginetypes.VirtualizationRemapMessage, err error) {
	workloads, err := c.store.ListNodeWorkloads(ctx, node.Name, nil)
	if err != nil {
		return
	}
	remapOpts := &enginetypes.VirtualizationRemapOptions{
		CPUAvailable:      node.CPU,
		CPUInit:           node.InitCPU,
		CPUShareBase:      int64(c.config.Scheduler.ShareBase),
		WorkloadResources: make(map[string]enginetypes.VirtualizationResource),
	}
	for _, workload := range workloads {
		remapOpts.WorkloadResources[workload.ID] = enginetypes.VirtualizationResource{
			CPU:      workload.CPU,
			Quota:    workload.CPUQuotaLimit,
			NUMANode: workload.NUMANode,
		}
	}
	ch, err = node.Engine.VirtualizationResourceRemap(ctx, remapOpts)
	return ch, errors.WithStack(err)
}

func (c *Calcium) doRemapResourceAndLog(ctx context.Context, logger log.Fields, node *types.Node) {
	log.Debugf(ctx, "[doRemapResourceAndLog] remap node %s", node.Name)
	ctx, cancel := context.WithTimeout(context.Background(), c.config.GlobalTimeout)
	defer cancel()
	logger = logger.WithField("Calcium", "doRemapResourceAndLog").WithField("nodename", node.Name)
	if ch, err := c.remapResource(ctx, node); logger.Err(err) == nil {
		for msg := range ch {
			logger.WithField("id", msg.ID).Err(msg.Error) // nolint:errcheck
		}
	}
}
