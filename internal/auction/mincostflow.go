package auction

import "math"

// flowNetwork is a min-cost flow network solved by successive shortest augmenting paths
// with node potentials.
//
// Two properties matter more here than raw speed. First, determinism: every choice, from
// the queue's tie-break to the order arcs are scanned, is decided by node and arc index, so
// the same canonical input always produces the same flow. Second, profitability: the solver
// stops augmenting as soon as the cheapest remaining path costs more than it returns, which
// turns "maximum flow" into "maximum surplus" without a separate pass.
//
// The inner loop is written for the batch sizes the specification targets. Arcs live in one
// flat slice addressed by index, and the priority queue is a typed binary heap rather than
// container/heap, whose any-typed Push allocates on every relaxation.
type flowNetwork struct {
	nodes int
	// heads[v] lists indices into arcs for arcs leaving v.
	heads [][]int32
	arcs  []flowArc
}

// flowArc is one directed arc. Its residual twin is always at index^1.
type flowArc struct {
	from, to int32
	capacity int64
	flow     int64
	cost     int64
}

func newFlowNetwork(nodes int) *flowNetwork {
	return &flowNetwork{nodes: nodes, heads: make([][]int32, nodes)}
}

// addEdge appends a directed arc and its residual twin, returning the arc's index so the
// caller can read the flow assigned to it afterwards.
func (f *flowNetwork) addEdge(from, to int, capacity, cost int64) int {
	index := int32(len(f.arcs))
	f.arcs = append(f.arcs,
		flowArc{from: int32(from), to: int32(to), capacity: capacity, cost: cost},
		flowArc{from: int32(to), to: int32(from), capacity: 0, cost: -cost},
	)
	f.heads[from] = append(f.heads[from], index)
	f.heads[to] = append(f.heads[to], index+1)
	return int(index)
}

// flowOn returns the flow assigned to the arc returned by addEdge.
func (f *flowNetwork) flowOn(arcIndex int) int64 { return f.arcs[arcIndex].flow }

// solveResult reports what an augmentation run did.
type solveResult struct {
	Flow          int64
	Cost          int64
	Augmentations int
	// LimitReached is true when the run stopped at maxAugmentations rather than because no
	// profitable path remained. The flow found so far is still feasible.
	LimitReached bool
}

// unreachable is the distance of a node no augmenting path reaches.
const unreachable int64 = math.MaxInt64 / 2

// solve pushes flow from source to sink while an augmenting path has negative cost, that
// is, while the next unit of flow is worth more than it costs.
//
// Costs may be negative, so Dijkstra alone would be wrong. Initial potentials come from a
// single relaxation pass in node order, which is exact because the construction numbers
// nodes topologically (source, lots, exposure nodes, bids, sink); after that every residual
// arc has a non-negative reduced cost and Dijkstra is valid.
func (f *flowNetwork) solve(source, sink int, maxAugmentations int) solveResult {
	potential := f.initialPotentials(source)

	var result solveResult
	dist := make([]int64, f.nodes)
	settled := make([]bool, f.nodes)
	prevArc := make([]int32, f.nodes)
	queue := newNodeQueue(f.nodes)

	for result.Augmentations < maxAugmentations {
		if !f.shortestPath(source, sink, potential, dist, settled, prevArc, queue) {
			return result
		}

		// Reduced costs hide the real price of the path; recover it before deciding whether
		// the augmentation is worth making.
		pathCost := dist[sink] + potential[sink] - potential[source]
		if pathCost >= 0 {
			return result
		}

		for v := range potential {
			if dist[v] < unreachable {
				potential[v] += dist[v]
			}
		}

		bottleneck := int64(math.MaxInt64)
		for v := int32(sink); v != int32(source); {
			arc := &f.arcs[prevArc[v]]
			if residual := arc.capacity - arc.flow; residual < bottleneck {
				bottleneck = residual
			}
			v = arc.from
		}

		for v := int32(sink); v != int32(source); {
			index := prevArc[v]
			f.arcs[index].flow += bottleneck
			f.arcs[index^1].flow -= bottleneck
			v = f.arcs[index].from
		}

		result.Flow += bottleneck
		result.Cost += bottleneck * pathCost
		result.Augmentations++
	}

	result.LimitReached = true
	return result
}

// initialPotentials computes shortest distances from the source over the original arcs by
// relaxing nodes in index order, which is valid because the network is a DAG numbered
// topologically before any flow is pushed.
func (f *flowNetwork) initialPotentials(source int) []int64 {
	potential := make([]int64, f.nodes)
	for v := range potential {
		potential[v] = unreachable
	}
	potential[source] = 0

	for v := range f.nodes {
		if potential[v] >= unreachable {
			continue
		}
		for _, index := range f.heads[v] {
			arc := &f.arcs[index]
			if arc.capacity == 0 {
				continue // residual twin, no capacity before any flow exists
			}
			if relaxed := potential[v] + arc.cost; relaxed < potential[arc.to] {
				potential[arc.to] = relaxed
			}
		}
	}

	// A node the source cannot reach gets a zero potential; it has no residual arcs into the
	// flow either way, and leaving it at "unreachable" would overflow the reduced-cost
	// arithmetic.
	for v := range potential {
		if potential[v] >= unreachable {
			potential[v] = 0
		}
	}
	return potential
}

// shortestPath runs Dijkstra over reduced costs, filling dist and prevArc. Ties are broken
// by node index so two runs on the same input always pick the same path.
func (f *flowNetwork) shortestPath(source, sink int, potential, dist []int64, settled []bool, prevArc []int32, queue *nodeQueue) bool {
	for v := range dist {
		dist[v] = unreachable
		settled[v] = false
		prevArc[v] = -1
	}
	dist[source] = 0

	queue.reset()
	queue.push(int32(source), 0)

	for {
		node, ok := queue.pop()
		if !ok {
			break
		}
		if settled[node] {
			continue
		}
		settled[node] = true
		if int(node) == sink {
			// The sink is settled: its distance is final, and nothing later can change the
			// path we are about to augment along.
			break
		}

		base := dist[node]
		for _, index := range f.heads[node] {
			arc := &f.arcs[index]
			if arc.capacity-arc.flow <= 0 {
				continue
			}
			next := base + arc.cost + potential[node] - potential[arc.to]
			if next < dist[arc.to] {
				dist[arc.to] = next
				prevArc[arc.to] = index
				queue.push(arc.to, next)
			}
		}
	}

	return dist[sink] < unreachable
}

// nodeQueue is a typed binary heap of (distance, node) pairs.
//
// container/heap would box every entry into an interface value, allocating once per
// relaxation; on a batch of a few thousand bids that allocation dominates the whole
// clearing. Ordering by distance and then node index keeps the tie-break published.
type nodeQueue struct {
	items []nodeDist
}

type nodeDist struct {
	dist int64
	node int32
}

func newNodeQueue(capacity int) *nodeQueue {
	return &nodeQueue{items: make([]nodeDist, 0, capacity)}
}

func (q *nodeQueue) reset() { q.items = q.items[:0] }

func (q *nodeQueue) less(a, b nodeDist) bool {
	if a.dist != b.dist {
		return a.dist < b.dist
	}
	return a.node < b.node
}

func (q *nodeQueue) push(node int32, dist int64) {
	q.items = append(q.items, nodeDist{dist: dist, node: node})
	child := len(q.items) - 1
	for child > 0 {
		parent := (child - 1) / 2
		if !q.less(q.items[child], q.items[parent]) {
			break
		}
		q.items[child], q.items[parent] = q.items[parent], q.items[child]
		child = parent
	}
}

func (q *nodeQueue) pop() (int32, bool) {
	if len(q.items) == 0 {
		return 0, false
	}
	top := q.items[0]
	last := len(q.items) - 1
	q.items[0] = q.items[last]
	q.items = q.items[:last]

	parent := 0
	for {
		left := 2*parent + 1
		if left >= len(q.items) {
			break
		}
		smallest := left
		if right := left + 1; right < len(q.items) && q.less(q.items[right], q.items[left]) {
			smallest = right
		}
		if !q.less(q.items[smallest], q.items[parent]) {
			break
		}
		q.items[parent], q.items[smallest] = q.items[smallest], q.items[parent]
		parent = smallest
	}
	return top.node, true
}
