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

// subscriptions maps filters to subscribers (K, a session in the broker) with a value per
// subscription (V, its options): a trie with one level per node.
type subscriptions[K comparable, V any] struct {
	mu   sync.RWMutex
	root *node[K, V]
	n    int
}

type node[K comparable, V any] struct {
	children map[string]*node[K, V]
	subs     map[K]V
}

func newSubscriptions[K comparable, V any]() *subscriptions[K, V] {
	return &subscriptions[K, V]{root: &node[K, V]{}}
}

// add subscribes s to filter with qos; it reports whether the subscription is new.
func (t *subscriptions[K, V]) add(filter string, s K, v V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.root
	for _, l := range strings.Split(filter, "/") {
		if n.children == nil {
			n.children = make(map[string]*node[K, V])
		}
		next := n.children[l]
		if next == nil {
			next = &node[K, V]{}
			n.children[l] = next
		}
		n = next
	}
	if n.subs == nil {
		n.subs = make(map[K]V)
	}
	_, existed := n.subs[s]
	n.subs[s] = v
	if !existed {
		t.n++
	}
	return !existed
}

// remove unsubscribes s from filter.
func (t *subscriptions[K, V]) remove(filter string, s K) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.removeLocked(filter, s)
}

func (t *subscriptions[K, V]) removeLocked(filter string, s K) bool {
	path := []*node[K, V]{t.root}
	levels := strings.Split(filter, "/")
	n := t.root
	for _, l := range levels {
		n = n.children[l]
		if n == nil {
			return false
		}
		path = append(path, n)
	}
	if _, ok := n.subs[s]; !ok {
		return false
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
	return true
}

// match calls fn once for every subscriber with a filter matching topic. A subscriber with several
// matching subscriptions gets their values combined with merge.
func (t *subscriptions[K, V]) match(topic string, merge func(a, b V) V, fn func(s K, v V)) {
	t.mu.RLock()
	found := make(map[K]V)
	levels := strings.Split(topic, "/")
	dollar := strings.HasPrefix(topic, "$")
	var walk func(n *node[K, V], i int)
	collect := func(n *node[K, V]) {
		for s, v := range n.subs {
			if old, ok := found[s]; ok {
				v = merge(old, v)
			}
			found[s] = v
		}
	}
	walk = func(n *node[K, V], i int) {
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
	for s, v := range found {
		fn(s, v)
	}
}

func (t *subscriptions[K, V]) count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.n
}
