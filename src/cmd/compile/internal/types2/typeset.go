// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

import (
	"bytes"
	"cmd/compile/internal/syntax"
	"fmt"
	"sort"
)

// ----------------------------------------------------------------------------
// API

// A TypeSet represents the type set of an interface.
type TypeSet struct {
	// TODO(gri) consider using a set for the methods for faster lookup
	methods []*Func // all methods of the interface; sorted by unique ID
	types   Type    // typically a *Union; nil means no type restrictions
}

// IsTop reports whether type set s is the top type set (corresponding to the empty interface).
func (s *TypeSet) IsTop() bool { return len(s.methods) == 0 && s.types == nil }

// IsMethodSet reports whether the type set s is described by a single set of methods.
func (s *TypeSet) IsMethodSet() bool { return s.types == nil && !s.IsComparable() }

// IsComparable reports whether each type in the set is comparable.
// TODO(gri) this is not correct - there may be s.types values containing non-comparable types
func (s *TypeSet) IsComparable() bool {
	_, m := s.LookupMethod(nil, "==")
	return m != nil
}

// NumMethods returns the number of methods available.
func (s *TypeSet) NumMethods() int { return len(s.methods) }

// Method returns the i'th method of type set s for 0 <= i < s.NumMethods().
// The methods are ordered by their unique ID.
func (s *TypeSet) Method(i int) *Func { return s.methods[i] }

// LookupMethod returns the index of and method with matching package and name, or (-1, nil).
func (s *TypeSet) LookupMethod(pkg *Package, name string) (int, *Func) {
	// TODO(gri) s.methods is sorted - consider binary search
	return lookupMethod(s.methods, pkg, name)
}

func (s *TypeSet) String() string {
	if s.IsTop() {
		return "⊤"
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range s.methods {
		if i > 0 {
			buf.WriteByte(';')
		}
		buf.WriteByte(' ')
		buf.WriteString(m.String())
	}
	if len(s.methods) > 0 && s.types != nil {
		buf.WriteByte(';')
	}
	if s.types != nil {
		buf.WriteByte(' ')
		writeType(&buf, s.types, nil, nil)
	}

	buf.WriteString(" }") // there was a least one method or type
	return buf.String()
}

// ----------------------------------------------------------------------------
// Implementation

// underIs reports whether f returned true for the underlying types of the
// enumerable types in the type set s. If the type set comprises all types
// f is called once with the top type; if the type set is empty, the result
// is false.
func (s *TypeSet) underIs(f func(Type) bool) bool {
	switch t := s.types.(type) {
	case nil:
		return f(theTop)
	default:
		return f(t)
	case *Union:
		return t.underIs(f)
	}
}

// topTypeSet may be used as type set for the empty interface.
var topTypeSet TypeSet

// computeTypeSet may be called with check == nil.
func computeTypeSet(check *Checker, pos syntax.Pos, ityp *Interface) *TypeSet {
	if ityp.tset != nil {
		return ityp.tset
	}

	// If the interface is not fully set up yet, the type set will
	// not be complete, which may lead to errors when using the the
	// type set (e.g. missing method). Don't compute a partial type
	// set (and don't store it!), so that we still compute the full
	// type set eventually. Instead, return the top type set and
	// let any follow-on errors play out.
	if !ityp.complete {
		return &topTypeSet
	}

	if check != nil && check.conf.Trace {
		// Types don't generally have position information.
		// If we don't have a valid pos provided, try to use
		// one close enough.
		if !pos.IsKnown() && len(ityp.methods) > 0 {
			pos = ityp.methods[0].pos
		}

		check.trace(pos, "type set for %s", ityp)
		check.indent++
		defer func() {
			check.indent--
			check.trace(pos, "=> %s ", ityp.typeSet())
		}()
	}

	// An infinitely expanding interface (due to a cycle) is detected
	// elsewhere (Checker.validType), so here we simply assume we only
	// have valid interfaces. Mark the interface as complete to avoid
	// infinite recursion if the validType check occurs later for some
	// reason.
	ityp.tset = new(TypeSet) // TODO(gri) is this sufficient?

	// Methods of embedded interfaces are collected unchanged; i.e., the identity
	// of a method I.m's Func Object of an interface I is the same as that of
	// the method m in an interface that embeds interface I. On the other hand,
	// if a method is embedded via multiple overlapping embedded interfaces, we
	// don't provide a guarantee which "original m" got chosen for the embedding
	// interface. See also issue #34421.
	//
	// If we don't care to provide this identity guarantee anymore, instead of
	// reusing the original method in embeddings, we can clone the method's Func
	// Object and give it the position of a corresponding embedded interface. Then
	// we can get rid of the mpos map below and simply use the cloned method's
	// position.

	var todo []*Func
	var seen objset
	var methods []*Func
	mpos := make(map[*Func]syntax.Pos) // method specification or method embedding position, for good error messages
	addMethod := func(pos syntax.Pos, m *Func, explicit bool) {
		switch other := seen.insert(m); {
		case other == nil:
			methods = append(methods, m)
			mpos[m] = pos
		case explicit:
			if check == nil {
				panic(fmt.Sprintf("%s: duplicate method %s", m.pos, m.name))
			}
			// check != nil
			var err error_
			err.errorf(pos, "duplicate method %s", m.name)
			err.errorf(mpos[other.(*Func)], "other declaration of %s", m.name)
			check.report(&err)
		default:
			// We have a duplicate method name in an embedded (not explicitly declared) method.
			// Check method signatures after all types are computed (issue #33656).
			// If we're pre-go1.14 (overlapping embeddings are not permitted), report that
			// error here as well (even though we could do it eagerly) because it's the same
			// error message.
			if check == nil {
				// check method signatures after all locally embedded interfaces are computed
				todo = append(todo, m, other.(*Func))
				break
			}
			// check != nil
			check.later(func() {
				if !check.allowVersion(m.pkg, 1, 14) || !Identical(m.typ, other.Type()) {
					var err error_
					err.errorf(pos, "duplicate method %s", m.name)
					err.errorf(mpos[other.(*Func)], "other declaration of %s", m.name)
					check.report(&err)
				}
			})
		}
	}

	for _, m := range ityp.methods {
		addMethod(m.pos, m, true)
	}

	// collect embedded elements
	var allTypes Type
	for i, typ := range ityp.embeddeds {
		// The embedding position is nil for imported interfaces
		// and also for interface copies after substitution (but
		// in that case we don't need to report errors again).
		var pos syntax.Pos // embedding position
		if ityp.embedPos != nil {
			pos = (*ityp.embedPos)[i]
		}
		var types Type
		switch t := under(typ).(type) {
		case *Interface:
			tset := computeTypeSet(check, pos, t)
			for _, m := range tset.methods {
				addMethod(pos, m, false) // use embedding position pos rather than m.pos
			}
			types = tset.types
		case *Union:
			// TODO(gri) combine with default case once we have
			//           converted all tests to new notation and we
			//           can report an error when we don't have an
			//           interface before go1.18.
			types = typ
		case *TypeParam:
			if check != nil && !check.allowVersion(check.pkg, 1, 18) {
				check.errorf(pos, "%s is a type parameter, not an interface", typ)
				continue
			}
			types = typ
		default:
			if typ == Typ[Invalid] {
				continue
			}
			if check != nil && !check.allowVersion(check.pkg, 1, 18) {
				check.errorf(pos, "%s is not an interface", typ)
				continue
			}
			types = typ
		}
		allTypes = intersect(allTypes, types)
	}
	ityp.embedPos = nil // not needed anymore (errors have been reported)

	// process todo's (this only happens if check == nil)
	for i := 0; i < len(todo); i += 2 {
		m := todo[i]
		other := todo[i+1]
		if !Identical(m.typ, other.typ) {
			panic(fmt.Sprintf("%s: duplicate method %s", m.pos, m.name))
		}
	}

	if methods != nil {
		sortMethods(methods)
		ityp.tset.methods = methods
	}
	ityp.tset.types = allTypes

	return ityp.tset
}

func sortMethods(list []*Func) {
	sort.Sort(byUniqueMethodName(list))
}

func assertSortedMethods(list []*Func) {
	if !debug {
		panic("internal error: assertSortedMethods called outside debug mode")
	}
	if !sort.IsSorted(byUniqueMethodName(list)) {
		panic("internal error: methods not sorted")
	}
}

// byUniqueMethodName method lists can be sorted by their unique method names.
type byUniqueMethodName []*Func

func (a byUniqueMethodName) Len() int           { return len(a) }
func (a byUniqueMethodName) Less(i, j int) bool { return a[i].less(&a[j].object) }
func (a byUniqueMethodName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }