package server

import (
	"container/heap"
	"slices"

	"github.com/gopcua/opcua/ua"
)

// DataValue StatusCode info bits, OPC UA Part 4 §7.38.1: InfoType and
// InfoBits in Table 176, the DataValue InfoBits in Table 177.
const (
	statusInfoTypeMask      ua.StatusCode = 0x00000C00 // InfoType, bits 10:11
	statusInfoTypeDataValue ua.StatusCode = 0x00000400 // InfoType DataValue (01)
	statusInfoBitsMask      ua.StatusCode = 0x000003FF // InfoBits, bits 0:9
	statusInfoBitOverflow   ua.StatusCode = 0x00000080 // Overflow, bit 7

	// StructureChanged (bit 15) and SemanticsChanged (bit 14). When a data
	// change notification with one of them set is discarded, the bit is set
	// on the next notification in the queue (Table 176).
	statusChangedBits ua.StatusCode = 0x0000C000
)

// defaultMaxMonitoredItemsQueueSize is the default upper bound for the queue
// size of a data MonitoredItem, the default of the
// ServerCapabilities.MaxMonitoredItemsQueueSize field.
const defaultMaxMonitoredItemsQueueSize = 5000

// defaultMaxNotificationsPerPublish is the default upper bound for the number
// of notifications in one Publish response, the default of the
// ServerCapabilities.MaxNotificationsPerPublish field. It bounds the number of
// notifications in a response when the client sets no limit or a large one;
// it does not bound the size in bytes.
const defaultMaxNotificationsPerPublish = 1000

// revisedQueueSize returns the queue size the server uses for a data
// MonitoredItem.
//
// Part 4 §7.21, Table 140: for a data MonitoredItem a requested size of 0 or
// 1 gives the default size 1; larger sizes may be limited by the server. The
// server does not implement event MonitoredItems, whose sizes Table 140
// revises differently, so every item is revised by this rule.
func revisedQueueSize(requested, limit uint32) uint32 {
	return min(max(requested, 1), max(limit, 1))
}

// queuedValue is one sampled value waiting for the next Publish response.
// seq numbers the values of all queues of a Subscription in the order they
// were sampled.
type queuedValue struct {
	seq   uint64
	value *ua.DataValue
}

// notificationQueue is the queue of a single data MonitoredItem.
//
// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13.1.5
type notificationQueue struct {
	clientHandle  uint32
	size          int
	discardOldest bool
	entries       []queuedValue

	// held is set while the MonitoringMode of the item is not Reporting:
	// its values stay queued and are not published (Part 4 §5.13.1.3).
	held bool
}

// setMode applies the MonitoringMode of the item to its queue. Part 4
// §5.13.1.3: only a reporting item publishes its queued values; a sampling
// item keeps them queued. §5.13.4.1: setting the mode to Disabled deletes all
// queued values. A value outside the enumeration (§7.23) is treated as
// Reporting.
func (q *notificationQueue) setMode(mode ua.MonitoringMode) {
	q.held = mode == ua.MonitoringModeDisabled || mode == ua.MonitoringModeSampling
	if mode == ua.MonitoringModeDisabled {
		clear(q.entries)
		q.entries = nil
	}
}

// push queues v according to the size and discard policy of the queue
// (Part 4 §5.13.1.5). The value that takes the place of a discarded one also
// inherits its StructureChanged and SemanticsChanged bits.
func (q *notificationQueue) push(v queuedValue) {
	switch {
	case q.size <= 1:
		// A queue of size one always holds the newest value. The discard
		// policy is ignored and the Overflow bit is not set.
		v.value = withChangedBits(v.value, q.entries...)
		q.entries = append(q.entries[:0], v)
	case len(q.entries) < q.size:
		q.entries = append(q.entries, v)
	case q.discardOldest:
		// Drop the oldest value and flag the value that is now oldest.
		dropped := q.entries[0]
		q.entries[0] = queuedValue{}
		q.entries = append(q.entries[1:], v)
		q.entries[0].value = withOverflow(withChangedBits(q.entries[0].value, dropped))
	default:
		// Replace the newest value and flag the new value.
		v.value = withOverflow(withChangedBits(v.value, q.entries[len(q.entries)-1]))
		q.entries[len(q.entries)-1] = v
	}
}

// resize applies new queue parameters. A queue that holds more values than
// the new size is trimmed following its discard policy (Part 4 §5.13.3.2,
// Table 66). With discardOldest the newest values are kept. Otherwise the
// oldest values are kept and the last of them is replaced by the newest
// value, which is how §7.21 describes discardOldest FALSE for a single new
// value; the spec does not spell out a trim by more than one.
//
// The value that stands in for the discarded ones, the oldest kept one with
// discardOldest and the newest one otherwise, gets the Overflow bit when the
// new size is larger than one, as for any value lost from a queue larger than
// one (§7.25.2, Table 161; §7.38.1, Table 177). Which value carries the bit
// on a trim is not specified; this follows §5.13.1.5. It also inherits the
// StructureChanged and SemanticsChanged bits of the discarded values. A queue
// of size one carries no Overflow bit.
func (q *notificationQueue) resize(size int, discardOldest bool) {
	q.size = size
	q.discardOldest = discardOldest

	if n := len(q.entries); n > max(size, 1) {
		keep := max(size, 1)
		var kept []queuedValue
		if discardOldest || keep == 1 {
			kept = slices.Clone(q.entries[n-keep:])
			kept[0].value = withChangedBits(kept[0].value, q.entries[:n-keep]...)
			if keep > 1 {
				kept[0].value = withOverflow(kept[0].value)
			}
		} else {
			kept = make([]queuedValue, 0, keep)
			kept = append(kept, q.entries[:keep-1]...)
			kept = append(kept, q.entries[n-1])
			kept[keep-1].value = withOverflow(withChangedBits(kept[keep-1].value, q.entries[keep-1:n-1]...))
		}
		q.entries = kept
	}

	if size <= 1 {
		for i := range q.entries {
			q.entries[i].value = withoutOverflow(q.entries[i].value)
		}
	}
}

// withOverflow returns a copy of v with the Overflow info bit set. The rest
// of the StatusCode, the value and the timestamps are unchanged. v itself is
// not modified since namespaces may hand out shared DataValues.
func withOverflow(v *ua.DataValue) *ua.DataValue {
	var dv ua.DataValue
	if v != nil {
		dv = *v
	}
	switch dv.Status & statusInfoTypeMask {
	case 0:
		dv.Status = dv.Status&^statusInfoBitsMask | statusInfoTypeDataValue | statusInfoBitOverflow
	case statusInfoTypeDataValue:
		dv.Status |= statusInfoBitOverflow
	default:
		// Reserved InfoType: the info bits have no defined meaning.
		return v
	}
	dv.EncodingMask |= ua.DataValueStatusCode
	return &dv
}

// withChangedBits returns v with the StructureChanged and SemanticsChanged
// bits of the discarded values added. v is copied when a bit is added, and
// returned as it is otherwise.
func withChangedBits(v *ua.DataValue, discarded ...queuedValue) *ua.DataValue {
	var bits ua.StatusCode
	for _, d := range discarded {
		if d.value != nil {
			bits |= d.value.Status & statusChangedBits
		}
	}
	if bits == 0 || (v != nil && v.Status&bits == bits) {
		return v
	}
	var dv ua.DataValue
	if v != nil {
		dv = *v
	}
	dv.Status |= bits
	dv.EncodingMask |= ua.DataValueStatusCode
	return &dv
}

// withoutOverflow returns a copy of v with the Overflow info bit cleared. The
// InfoType is reset when no other DataValue info bit remains set.
func withoutOverflow(v *ua.DataValue) *ua.DataValue {
	if v == nil || v.Status&statusInfoTypeMask != statusInfoTypeDataValue || v.Status&statusInfoBitOverflow == 0 {
		return v
	}
	dv := *v
	dv.Status &^= statusInfoBitOverflow
	if dv.Status&statusInfoBitsMask == 0 {
		dv.Status &^= statusInfoTypeMask
	}
	return &dv
}

// createQueue creates the queue of a new MonitoredItem with the given id and
// MonitoringMode.
func (s *Subscription) createQueue(id, clientHandle, size uint32, discardOldest bool, mode ua.MonitoringMode) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.queues == nil {
		s.queues = map[uint32]*notificationQueue{}
	}
	q := &notificationQueue{
		clientHandle:  clientHandle,
		size:          int(size),
		discardOldest: discardOldest,
	}
	q.setMode(mode)
	s.queues[id] = q
}

// setQueueMode applies a new MonitoringMode to the queue of the MonitoredItem
// with the given id. It does nothing if the item has no queue.
func (s *Subscription) setQueueMode(id uint32, mode ua.MonitoringMode) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if q, ok := s.queues[id]; ok {
		q.setMode(mode)
	}
}

// updateQueue applies new parameters to the queue of the MonitoredItem with
// the given id. It reports false if the item has no queue, because it was
// deleted, and never creates one.
func (s *Subscription) updateQueue(id, clientHandle, size uint32, discardOldest bool) bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	q, ok := s.queues[id]
	if !ok {
		return false
	}
	q.clientHandle = clientHandle
	q.resize(int(size), discardOldest)
	return true
}

// removeQueue discards the queue of the MonitoredItem with the given id and
// all values in it.
func (s *Subscription) removeQueue(id uint32) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	delete(s.queues, id)
}

// enqueue adds a sampled value to the queue of the MonitoredItem with the
// given id. Values for an item without a queue are dropped.
func (s *Subscription) enqueue(id uint32, v *ua.DataValue) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	q, ok := s.queues[id]
	if !ok {
		return
	}
	s.queueSeq++
	q.push(queuedValue{seq: s.queueSeq, value: v})
}

// hasQueued reports whether any MonitoredItem of the subscription has a
// value waiting to be published. Values held by an item that is not
// reporting do not count.
func (s *Subscription) hasQueued() bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	for _, q := range s.queues {
		if !q.held && len(q.entries) > 0 {
			return true
		}
	}
	return false
}

// drainQueues takes the queued values of the subscription and returns one
// MonitoredItemNotification per value. The values of each item keep their
// queue order (Part 4 §5.13.1.5, §7.25.1); across items they are in the order
// they were sampled, which the spec does not require. At most limit values
// are taken; a limit of zero takes all of them (§5.14.2.2, Table 82,
// maxNotificationsPerPublish). The values that do not fit stay queued, and
// more reports whether any are left (§5.14.1.2; moreNotifications in
// §5.14.5.2, Table 89). The queues of items that are not reporting are left
// as they are (§5.13.1.3).
func (s *Subscription) drainQueues(limit uint32) (items []*ua.MonitoredItemNotification, more bool) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	// Merge the queues by the sequence number of their oldest value: every
	// queue is looked at once, and of the queued values only those taken.
	var heads queueHeads
	var total int
	for _, q := range s.queues {
		if !q.held && len(q.entries) > 0 {
			heads = append(heads, &queueHead{q: q})
			total += len(q.entries)
		}
	}
	if total == 0 {
		return nil, false
	}
	n := total
	if limit > 0 && uint64(limit) < uint64(total) {
		n = int(limit)
	}
	touched := slices.Clone(heads)
	heap.Init(&heads)

	items = make([]*ua.MonitoredItemNotification, 0, n)
	for len(items) < n {
		h := heads[0]
		items = append(items, &ua.MonitoredItemNotification{
			ClientHandle: h.q.clientHandle,
			Value:        h.q.entries[h.next].value,
		})
		h.next++
		if h.next == len(h.q.entries) {
			heap.Pop(&heads)
		} else {
			heap.Fix(&heads, 0)
		}
	}

	// The values of each queue are in sample order, so the values taken
	// from a queue are always at its front. Clear them so that the queue
	// does not keep them alive.
	for _, h := range touched {
		switch {
		case h.next == len(h.q.entries):
			h.q.entries = nil
		case h.next > 0:
			clear(h.q.entries[:h.next])
			h.q.entries = h.q.entries[h.next:]
		}
	}
	return items, len(heads) > 0
}

// queueHead points at the oldest value of a queue that drainQueues has not
// taken yet.
type queueHead struct {
	q    *notificationQueue
	next int
}

// queueHeads is a min-heap of queues ordered by the sequence number of the
// value each head points at.
type queueHeads []*queueHead

func (h queueHeads) Len() int { return len(h) }
func (h queueHeads) Less(i, j int) bool {
	return h[i].q.entries[h[i].next].seq < h[j].q.entries[h[j].next].seq
}
func (h queueHeads) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *queueHeads) Push(x any)   { *h = append(*h, x.(*queueHead)) }
func (h *queueHeads) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
