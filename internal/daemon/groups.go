package daemon

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"agentdeck/internal/model"
)

// sortSessions orders the list by Order (then age). Call with d.mu held.
func (d *Daemon) sortSessions() {
	sort.SliceStable(d.sessions, func(i, j int) bool {
		a, b := d.sessions[i], d.sessions[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.CreatedAt < b.CreatedAt
	})
}

// nextOrder puts a new session at the end. Call with d.mu held.
func (d *Daemon) nextOrder() int {
	n := 0
	for _, s := range d.sessions {
		n = max(n, s.Order)
	}
	return n + 1
}

// SetGroup puts a session in a group ("" = no group).
func (d *Daemon) SetGroup(id, group string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(id)
	if err != nil {
		return err
	}
	d.setGroupLocked(s, group)
	return nil
}

func (d *Daemon) setGroupLocked(s *model.Session, group string) {
	group = strings.Join(strings.Fields(group), " ")
	if s.Group == group {
		return
	}
	s.Group = group
	s.Order = d.nextOrder() // joins at the end of its new group
	d.sortSessions()
	d.save(s)
	d.changed()
}

// RenameGroup renames a group (all its sessions).
func (d *Daemon) RenameGroup(old, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	name = strings.Join(strings.Fields(name), " ")
	if old == "" {
		return errors.New("no group")
	}
	for _, s := range d.sessions {
		if s.Group == old {
			s.Group = name
			d.save(s)
		}
	}
	list := d.collapsed()
	for i, g := range list {
		if g == old {
			list[i] = name
		}
	}
	d.setCollapsed(list)
	d.changed()
	return nil
}

// Move moves a session up (delta < 0) or down among the sessions of its
// group.
func (d *Daemon) Move(id string, delta int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(id)
	if err != nil {
		return err
	}
	var same []*model.Session
	pos := -1
	for _, x := range d.sessions {
		if x.Group == s.Group {
			if x == s {
				pos = len(same)
			}
			same = append(same, x)
		}
	}
	to := pos + delta
	if to < 0 || to >= len(same) {
		return nil // already first or last
	}
	// Renumber the whole list so orders are distinct, then swap.
	for i, x := range d.sessions {
		x.Order = i + 1
	}
	same[pos].Order, same[to].Order = same[to].Order, same[pos].Order
	d.sortSessions()
	for _, x := range d.sessions {
		d.save(x)
	}
	d.changed()
	return nil
}

// Collapse folds or unfolds a group in the list.
func (d *Daemon) Collapse(group string, collapsed bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var list []string
	for _, g := range d.collapsed() {
		if g != group {
			list = append(list, g)
		}
	}
	if collapsed {
		list = append(list, group)
	}
	d.setCollapsed(list)
	d.changed()
	return nil
}

// collapsed lists the folded groups. Call with d.mu held.
func (d *Daemon) collapsed() []string {
	var list []string
	_ = json.Unmarshal([]byte(d.st.Get("collapsed")), &list)
	return list
}

func (d *Daemon) setCollapsed(list []string) {
	b, _ := json.Marshal(list)
	_ = d.st.Set("collapsed", string(b))
}
