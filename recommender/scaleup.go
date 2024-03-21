package recommender

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"

	scalesim "github.com/elankath/scaler-simulator"
	"github.com/elankath/scaler-simulator/webutil"
	"github.com/gardener/gardener/pkg/apis/core/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

/*
	for {
		unscheduledPods = determine unscheduled pods
		if noUnscheduledPods then exit early
		- runSimulation
 		  - Start a go-routine for each of candidate nodePool which are eligible
				- eligibility: max is not yet reached for that nodePool
              For each go-routine:
                Setup:
                    - create a unique label that will get added to all nodes and pods
                	- copy previous winner nodes and a taint.
                	- copy the deployed pods with node names assigned and add toleration to the taint.
	            - scale up one node, add a taint and only copy of pods will have toleration to that taint.
                - copy of unscheduled pods, add a toleration for this taint.
                - wait for pods to be scheduled
                - compute node score.
	}
*/

type StrategyWeights struct {
	LeastWaste float64
	LeastCost  float64
}

type Recommendation map[string]int

type Recommender struct {
	engine          scalesim.Engine
	shootNodes      []corev1.Node
	scenarioName    string
	shootName       string
	strategyWeights StrategyWeights
	logWriter       http.ResponseWriter
}

type runResult struct {
	result          scalesim.NodeRunResult
	unscheduledPods []corev1.Pod
	err             error
}

func NewRecommender(engine scalesim.Engine, shootNodes []corev1.Node, scenarioName, shootName string, strategyWeights StrategyWeights, logWriter http.ResponseWriter) *Recommender {
	return &Recommender{
		engine:          engine,
		shootNodes:      shootNodes,
		scenarioName:    scenarioName,
		shootName:       shootName,
		strategyWeights: strategyWeights,
		logWriter:       logWriter,
	}
}

func (r *Recommender) Run(ctx context.Context) (Recommendation, error) {
	recommendation := make(Recommendation)
	unscheduledPods, err := r.engine.VirtualClusterAccess().ListPods(ctx)
	if err != nil {
		return recommendation, err
	}
	var runNumber int
	shoot, err := r.getShoot()
	if err != nil {
		webutil.InternalError(r.logWriter, err)
		return recommendation, err
	}
	var winningNodeResult *scalesim.NodeRunResult
	for {
		runNumber++
		webutil.Log(r.logWriter, fmt.Sprintf("scale-up recommender run #%d started...", runNumber))
		if len(unscheduledPods) == 0 {
			webutil.Log(r.logWriter, "All pods are scheduled. Exiting the loop...")
			break
		}
		winningNodeResult, unscheduledPods, err = r.runSimulation(ctx, shoot, unscheduledPods, runNumber)
		if err != nil {
			webutil.Log(r.logWriter, fmt.Sprintf("Unable to get eligible node pools for shoot %s, err: %v", shoot.Name, err))
			break
		}
		if winningNodeResult == nil {
			webutil.Log(r.logWriter, fmt.Sprintf("scale-up recommender run #%d, no winner could be identified. This will happen when no pods could be assgined. No more runs are required, exiting early", runCounter))
			break
		}
		webutil.Log(r.logWriter, fmt.Sprintf("For scale-up recommender run #%d, winning score is: %v", runNumber, winningNodeResult))
	}

	return recommendation, nil
}

func (r *Recommender) getShoot() (*v1beta1.Shoot, error) {
	shoot, err := r.engine.ShootAccess(r.shootName).GetShootObj()
	if err != nil {
		return nil, err
	}
	return shoot, nil
}

// TODO: sync existing nodes and pods deployed on them. DO NOT TAINT THESE NODES.
// eg:- 1 node(A) existing in zone a. Any node can only fit 2 pods.
// deployment 6 replicas, tsc zone, minDomains 3
// 1 pod will get assigned to A. 5 pending. 3 Nodes will be scale up. (1-a, 1-b, 1-c)
// if you count existing nodes and pods, then only 2 nodes are needed.

func (r *Recommender) runSimulation(ctx context.Context, shoot *v1beta1.Shoot, pods []corev1.Pod, runNum int) (*scalesim.NodeRunResult, []corev1.Pod, error) {
	/*
		    1. getEligibleNodePools
			2. For each nodePool, start a go routine. Each go routine will return a node score.
			3. Collect the scores and return

			Inside each go routine:-
				1. Setup:-
					 - create a unique label that will get added to all nodes and pods (for helping in clean up)
				     - copy previous winner nodes and add a taint.
		             - copy the deployed pods with node names assigned and add a toleration to the taint.
				2. For each zone in the nodePool:-
					- scale up one node
					- wait for assignment of pods (5 sec delay),
					- calculate the score.
			    	- Reset the state
			    3. Compute the winning score for this nodePool and push to the result channel.
	*/
	eligibleNodePools, err := r.getEligibleNodePools(ctx, shoot)
	if err != nil {
		return nil, nil, err
	}
	var results []runResult

	resultCh := make(chan runResult, len(eligibleNodePools))
	go r.triggerNodePoolSimulations(ctx, eligibleNodePools, resultCh, runNum)

	// label, taint, result chan, error chan, close chan
	var errs error
	for result := range resultCh {
		if result.err != nil {
			_ = errors.Join(errs, err)
		} else {
			results = append(results, result)
		}
	}
	if errs != nil {
		return nil, nil, err
	}
	winningResult := getWinner(results)
	return &winningResult.result, winningResult.unscheduledPods, nil
}

func (r *Recommender) triggerNodePoolSimulations(ctx context.Context, nodePools []scalesim.NodePool, resultCh chan runResult, runNum int) {
	wg := &sync.WaitGroup{}
	for _, nodePool := range nodePools {
		wg.Add(1)
		go r.runSimulationForNodePool(ctx, wg, nodePool, resultCh, runNum)
	}
	wg.Wait()
	close(resultCh)
}

func (r *Recommender) runSimulationForNodePool(ctx context.Context, wg *sync.WaitGroup, nodePool scalesim.NodePool, resultCh chan runResult, runNum int) {
	defer wg.Done()
	runRes := runResult{}

	labelKey := "app.kubernetes.io/simulation-run"
	labelValue := nodePool.Name + "-" + strconv.Itoa(runNum)

	nodes, err := r.engine.VirtualClusterAccess().ListNodes(ctx)
	if err != nil {
		runRes.err = err
		resultCh <- runRes
		return
	}
	var NodeList []*corev1.Node
	for _, node := range nodes {
		nodeCopy := node.DeepCopy()
		nodeCopy.Name = node.Name + "SimRun-" + labelValue
		nodeCopy.Labels[labelKey] = labelValue
		NodeList = append(NodeList, nodeCopy)
	}
	r.engine.VirtualClusterAccess().AddNodes(ctx, NodeList...)

	pods, err := r.engine.VirtualClusterAccess().ListPods(ctx)
	if err != nil {
		runRes.err = err
		resultCh <- runRes
		return
	}

}

func (r *Recommender) getEligibleNodePools(ctx context.Context, shoot *v1beta1.Shoot) ([]scalesim.NodePool, error) {
	eligibleNodePools := make([]scalesim.NodePool, 0, len(shoot.Spec.Provider.Workers))
	for _, worker := range shoot.Spec.Provider.Workers {
		nodes, err := r.engine.VirtualClusterAccess().ListNodesInNodePool(ctx, worker.Name)
		if err != nil {
			return nil, err
		}
		if int32(len(nodes)) >= worker.Maximum {
			continue
		}
		nodePool := scalesim.NodePool{
			Name:        worker.Name,
			Zones:       worker.Zones,
			Max:         worker.Maximum,
			Current:     int32(len(nodes)),
			MachineType: worker.Machine.Type,
		}
		eligibleNodePools = append(eligibleNodePools, nodePool)
	}
	return eligibleNodePools, nil
}

func getWinner(results []runResult) runResult {
	var winner runResult
	minScore := math.MaxFloat64
	for _, v := range results {
		if v.result.CumulativeScore < minScore {
			winner = v
			minScore = v.result.CumulativeScore
		}
	}
	return winner
}

func (r *Recommender) logError(err error) {
	webutil.Log(r.logWriter, "Execution of scenario: "+r.scenarioName+" completed with error: "+err.Error())
}
