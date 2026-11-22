package mqtt

import (
	"strings"
	"sync"
)

// validTopicName reports whether name can be published to: not empty, no wildcards.
func validTopicName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "+#")
}

// validFilter reports whether filter is a valid subscription filter: '+' must fill a whole level,
// '#' must fill the last level.
func validFilter(filter string) bool {
	if filter == "" {
		return false
	}
	levels := strings.Split(filter, "/")
	for i, l := range levels {
		switch {
		case l == "#":
			if i != len(levels)-1 {
				return false
			}
		case l == "+":
		case strings.ContainsAny(l, "+#"):
			return false
		}
	}
	return true
}

// matchFilter reports whether topic matches filter (both valid). Topics starting with '$' are not
// matched by filters starting with a wildcard (section 4.7.2).
func matchFilter(filter, topic string) bool {
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(filter, "+") || strings.HasPrefix(filter, "#")) {
		return false
	}
	f, t := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, l := range f {
		if l == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if l != "+" && l != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

// subscriptions maps filters to subscribers (K, a session in the broker): a trie with one level per node.
type subscriptions[K comparable] struct {
	mu   sync.RWMutex
	root *node[K]
	n    int
}

type node[K comparable] struct {
	children map[string]*node[K]
	subs     map[K]byte // subscriber -> granted QoS
}

func newSubscriptions[K comparable]() *subscriptions[K] { return &subscriptions[K]{root: &node[K]{}} }

// add subscribes s to filter with qos; it reports whether the subscription is new.
func (t *subscriptions[K]) add(filter string, s K, qos byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.root
	for _, l := range strings.Split(filter, "/") {
		if n.children == nil {
			n.children = make(map[string]*node[K])
		}
		next := n.children[l]
		if next == nil {
			next = &node[K]{}
			n.children[l] = next
		}
		n = next
	}
	if n.subs == nil {
		n.subs = make(map[K]byte)
	}
	_, existed := n.subs[s]
	n.subs[s] = qos
	if !existed {
		t.n++
	}
	return !existed
}

// remove unsubscribes s from filter.
func (t *subscriptions[K]) remove(filter string, s K) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeLocked(filter, s)
}

func (t *subscriptions[K]) removeLocked(filter string, s K) {
	path := []*node[K]{t.root}
	levels := strings.Split(filter, "/")
	n := t.root
	for _, l := range levels {
		n = n.children[l]
		if n == nil {
			return
		}
		path = append(path, n)
	}
	if _, ok := n.subs[s]; !ok {
		return
	}
	delete(n.subs, s)
	t.n--
	// Prune nodes left empty
	for i := len(levels) - 1; i >= 0; i-- {
		child := path[i+1]
		if len(child.subs) > 0 || len(child.children) > 0 {
			break
		}
		delete(path[i].children, levels[i])
	}
}

// match calls fn for every session subscribed to a filter matching topic, with the highest
// granted QoS among its matching subscriptions.
func (t *subscriptions[K]) match(topic string, fn func(s K, qos byte)) {
	t.mu.RLock()
	found := make(map[K]byte)
	levels := strings.Split(topic, "/")
	dollar := strings.HasPrefix(topic, "$")
	var walk func(n *node[K], i int)
	collect := func(n *node[K]) {
		for s, q := range n.subs {
			if old, ok := found[s]; !ok || q > old {
				found[s] = q
			}
		}
	}
	walk = func(n *node[K], i int) {
		if i == len(levels) {
			collect(n)
			if hash := n.children["#"]; hash != nil { // "a/#" also matches "a"
				collect(hash)
			}
			return
		}
		wild := !(dollar && i == 0) // no wildcard match for the first level of a $ topic
		if wild {
			if hash := n.children["#"]; hash != nil {
				collect(hash)
			}
			if plus := n.children["+"]; plus != nil {
				walk(plus, i+1)
			}
		}
		if next := n.children[levels[i]]; next != nil {
			walk(next, i+1)
		}
	}
	walk(t.root, 0)
	t.mu.RUnlock()
	for s, q := range found {
		fn(s, q)
	}
}

func (t *subscriptions[K]) count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.n
}
