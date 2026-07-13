package proc

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type grouptest struct {
	grouper *Grouper
	procs   Iter
	want    GroupByName
}

//func (gt grouptest) run(c *C) {
//	_, err := gt.grouper.Update(gt.procs)
//	c.Assert(err, IsNil)
//
//	got := gt.grouper.curgroups()
//	c.Check(got, DeepEquals, gt.want, Commentf("diff %s", pretty.Compare(got, gt.want)))
//}

func rungroup(t *testing.T, gr *Grouper, procs Iter) GroupByName {
	_, groups, err := gr.Update(procs)
	if err != nil {
		t.Fatalf("group.Update error: %v", err)
	}

	return groups
}

// TestGrouperBasic tests core Update/curgroups functionality on single-proc
// groups: the grouper adds to counts and updates the other tracked metrics like
// Memory.
func TestGrouperBasic(t *testing.T) {
	p1, p2 := 1, 2
	n1, n2 := "g1", "g2"
	starttime := time.Unix(0, 0).UTC()

	tests := []struct {
		procs []IDInfo
		want  GroupByName
	}{
		{
			[]IDInfo{
				piinfost(p1, n1, Counts{1, 2, 3, 4, 5, 6, 0, 0}, Memory{7, 8, 0, 0, 0},
					Filedesc{4, 400}, 2, States{Other: 1}),
				piinfost(p2, n2, Counts{2, 3, 4, 5, 6, 7, 0, 0}, Memory{8, 9, 0, 0, 0},
					Filedesc{40, 400}, 3, States{Waiting: 1}),
			},
			GroupByName{
				"g1": Group{Counts{}, States{Other: 1}, msi{}, 1, Memory{7, 8, 0, 0, 0}, starttime,
					4, 0.01, 2, nil, "", false, true},
				"g2": Group{Counts{}, States{Waiting: 1}, msi{}, 1, Memory{8, 9, 0, 0, 0}, starttime,
					40, 0.1, 3, nil, "", false, true},
			},
		},
		{
			[]IDInfo{
				piinfost(p1, n1, Counts{2, 3, 4, 5, 6, 7, 0, 0},
					Memory{6, 7, 0, 0, 0}, Filedesc{100, 400}, 4, States{Zombie: 1}),
				piinfost(p2, n2, Counts{4, 5, 6, 7, 8, 9, 0, 0},
					Memory{9, 8, 0, 0, 0}, Filedesc{400, 400}, 2, States{Running: 1}),
			},
			GroupByName{
				"g1": Group{Counts{1, 1, 1, 1, 1, 1, 0, 0}, States{Zombie: 1}, msi{}, 1,
					Memory{6, 7, 0, 0, 0}, starttime, 100, 0.25, 4, nil, "", false, true},
				"g2": Group{Counts{2, 2, 2, 2, 2, 2, 0, 0}, States{Running: 1}, msi{}, 1,
					Memory{9, 8, 0, 0, 0}, starttime, 400, 1, 2, nil, "", false, true},
			},
		},
	}

	gr := NewGrouper(newNamer(n1, n2), false, false, false, 0, false, false)
	for i, tc := range tests {
		got := rungroup(t, gr, procInfoIter(tc.procs...))
		if diff := cmp.Diff(got, tc.want); diff != "" {
			t.Errorf("%d: curgroups differs: (-got +want)\n%s", i, diff)
		}
	}
}

// TestGrouperProcJoin tests the appearance of a new process in a group,
// and that all procs metrics contribute to a group.
func TestGrouperProcJoin(t *testing.T) {
	p1, p2 := 1, 2
	n1, n2 := "g1", "g1"
	starttime := time.Unix(0, 0).UTC()

	tests := []struct {
		procs []IDInfo
		want  GroupByName
	}{
		{
			[]IDInfo{
				piinfo(p1, n1, Counts{1, 2, 3, 4, 5, 6, 0, 0}, Memory{3, 4, 0, 0, 0}, Filedesc{4, 400}, 2),
			},
			GroupByName{
				"g1": Group{Counts{}, States{}, msi{}, 1, Memory{3, 4, 0, 0, 0}, starttime, 4, 0.01, 2, nil, "", false, true},
			},
		}, {
			// The counts for pid2 won't be factored into the total yet because we only add
			// to counts starting with the second time we see a proc. Memory and FDs are
			// affected though.
			[]IDInfo{
				piinfost(p1, n1, Counts{3, 4, 5, 6, 7, 8, 0, 0},
					Memory{3, 4, 0, 0, 0}, Filedesc{4, 400}, 2, States{Running: 1}),
				piinfost(p2, n2, Counts{1, 1, 1, 1, 1, 1, 0, 0},
					Memory{1, 2, 0, 0, 0}, Filedesc{40, 400}, 3, States{Sleeping: 1}),
			},
			GroupByName{
				"g1": Group{Counts{2, 2, 2, 2, 2, 2, 0, 0}, States{Running: 1, Sleeping: 1}, msi{}, 2,
					Memory{4, 6, 0, 0, 0}, starttime, 44, 0.1, 5, nil, "", false, true},
			},
		}, {
			[]IDInfo{
				piinfost(p1, n1, Counts{4, 5, 6, 7, 8, 9, 0, 0},
					Memory{1, 5, 0, 0, 0}, Filedesc{4, 400}, 2, States{Running: 1}),
				piinfost(p2, n2, Counts{2, 2, 2, 2, 2, 2, 0, 0},
					Memory{2, 4, 0, 0, 0}, Filedesc{40, 400}, 3, States{Running: 1}),
			},
			GroupByName{
				"g1": Group{Counts{4, 4, 4, 4, 4, 4, 0, 0}, States{Running: 2}, msi{}, 2,
					Memory{3, 9, 0, 0, 0}, starttime, 44, 0.1, 5, nil, "", false, true},
			},
		},
	}

	gr := NewGrouper(newNamer(n1), false, false, false, 0, false, false)
	for i, tc := range tests {
		got := rungroup(t, gr, procInfoIter(tc.procs...))
		if diff := cmp.Diff(got, tc.want); diff != "" {
			t.Errorf("%d: curgroups differs: (-got +want)\n%s", i, diff)
		}
	}
}

// TestGrouperNonDecreasing tests the disappearance of a process.  Its previous
// contribution to the counts should not go away when that happens if removeEmptyGroups is false.
func TestGrouperNonDecreasing(t *testing.T) {
	p1, p2 := 1, 2
	n1, n2 := "g1", "g1"
	starttime := time.Unix(0, 0).UTC()

	tests := []struct {
		procs []IDInfo
		want  GroupByName
	}{
		{
			[]IDInfo{
				piinfo(p1, n1, Counts{3, 4, 5, 6, 7, 8, 0, 0}, Memory{3, 4, 0, 0, 0}, Filedesc{4, 400}, 2),
				piinfo(p2, n2, Counts{1, 1, 1, 1, 1, 1, 0, 0}, Memory{1, 2, 0, 0, 0}, Filedesc{40, 400}, 3),
			},
			GroupByName{
				"g1": Group{Counts{}, States{}, msi{}, 2, Memory{4, 6, 0, 0, 0}, starttime, 44, 0.1, 5, nil, "", false, true},
			},
		}, {
			[]IDInfo{
				piinfo(p1, n1, Counts{4, 5, 6, 7, 8, 9, 0, 0}, Memory{1, 5, 0, 0, 0}, Filedesc{4, 400}, 2),
			},
			GroupByName{
				"g1": Group{Counts{1, 1, 1, 1, 1, 1, 0, 0}, States{}, msi{}, 1, Memory{1, 5, 0, 0, 0}, starttime, 4, 0.01, 2, nil, "", false, true},
			},
		}, {
			[]IDInfo{},
			GroupByName{
				"g1": Group{Counts{1, 1, 1, 1, 1, 1, 0, 0}, States{}, nil, 0, Memory{}, time.Time{}, 0, 0, 0, nil, "", false, false},
			},
		},
	}

	gr := NewGrouper(newNamer(n1), false, false, false, 0, false, false)
	for i, tc := range tests {
		got := rungroup(t, gr, procInfoIter(tc.procs...))
		if diff := cmp.Diff(got, tc.want); diff != "" {
			t.Errorf("%d: curgroups differs: (-got +want)\n%s", i, diff)
		}
	}
}

// TestGrouperNonDecreasing tests the disappearance of a process.
// We want the group to disappear if removeEmptyGroups is true.
func TestGrouperRemoveEmptyGroups(t *testing.T) {
	p1, p2 := 1, 2
	n1, n2 := "g1", "g2"
	starttime := time.Unix(0, 0).UTC()

	tests := []struct {
		procs []IDInfo
		want  GroupByName
	}{
		{
			[]IDInfo{
				piinfo(p1, n1, Counts{3, 4, 5, 6, 7, 8, 0, 0}, Memory{3, 4, 0, 0, 0}, Filedesc{4, 400}, 2),
				piinfo(p2, n2, Counts{1, 1, 1, 1, 1, 1, 0, 0}, Memory{1, 2, 0, 0, 0}, Filedesc{40, 400}, 3),
			},
			GroupByName{
				n1: Group{Counts{}, States{}, msi{}, 1, Memory{3, 4, 0, 0, 0}, starttime, 4, 0.01, 2, nil, "", false, true},
				n2: Group{Counts{}, States{}, msi{}, 1, Memory{1, 2, 0, 0, 0}, starttime, 40, 0.1, 3, nil, "", false, true},
			},
		}, {
			[]IDInfo{
				piinfo(p1, n1, Counts{4, 5, 6, 7, 8, 9, 0, 0}, Memory{1, 5, 0, 0, 0}, Filedesc{4, 400}, 2),
			},
			GroupByName{
				n1: Group{Counts{1, 1, 1, 1, 1, 1, 0, 0}, States{}, msi{}, 1, Memory{1, 5, 0, 0, 0}, starttime, 4, 0.01, 2, nil, "", false, true},
			},
		}, {
			[]IDInfo{},
			GroupByName{},
		},
	}

	gr := NewGrouper(newNamer(n1, n2), false, false, false, 0, false, true)
	for i, tc := range tests {
		got := rungroup(t, gr, procInfoIter(tc.procs...))
		if diff := cmp.Diff(got, tc.want); diff != "" {
			t.Errorf("%d: curgroups differs: (-got +want)\n%s", i, diff)
		}
	}
}

func TestGrouperThreads(t *testing.T) {
	p, n, tm := 1, "g1", time.Unix(0, 0).UTC()

	tests := []struct {
		proc IDInfo
		want GroupByName
	}{
		{
			piinfot(p, n, Counts{}, Memory{}, Filedesc{1, 1}, []Thread{
				{ThreadID(ID{p, 0}), "t1", Counts{1, 2, 3, 4, 5, 6, 0, 0}, "", States{}},
				{ThreadID(ID{p + 1, 0}), "t2", Counts{1, 1, 1, 1, 1, 1, 0, 0}, "", States{}},
			}),
			GroupByName{
				"g1": Group{Counts{}, States{}, msi{}, 1, Memory{}, tm, 1, 1, 2, []Threads{
					Threads{"t1", 1, Counts{}},
					Threads{"t2", 1, Counts{}},
				}, "", false, true},
			},
		}, {
			piinfot(p, n, Counts{}, Memory{}, Filedesc{1, 1}, []Thread{
				{ThreadID(ID{p, 0}), "t1", Counts{2, 3, 4, 5, 6, 7, 0, 0}, "", States{}},
				{ThreadID(ID{p + 1, 0}), "t2", Counts{2, 2, 2, 2, 2, 2, 0, 0}, "", States{}},
				{ThreadID(ID{p + 2, 0}), "t2", Counts{1, 1, 1, 1, 1, 1, 0, 0}, "", States{}},
			}),
			GroupByName{
				"g1": Group{Counts{}, States{}, msi{}, 1, Memory{}, tm, 1, 1, 3, []Threads{
					Threads{"t1", 1, Counts{1, 1, 1, 1, 1, 1, 0, 0}},
					Threads{"t2", 2, Counts{1, 1, 1, 1, 1, 1, 0, 0}},
				}, "", false, true},
			},
		}, {
			piinfot(p, n, Counts{}, Memory{}, Filedesc{1, 1}, []Thread{
				{ThreadID(ID{p + 1, 0}), "t2", Counts{4, 4, 4, 4, 4, 4, 0, 0}, "", States{}},
				{ThreadID(ID{p + 2, 0}), "t2", Counts{2, 3, 4, 5, 6, 7, 0, 0}, "", States{}},
			}),
			GroupByName{
				"g1": Group{Counts{}, States{}, msi{}, 1, Memory{}, tm, 1, 1, 2, []Threads{
					Threads{"t2", 2, Counts{4, 5, 6, 7, 8, 9, 0, 0}},
				}, "", false, true},
			},
		},
	}

	opts := cmpopts.SortSlices(lessThreads)
	gr := NewGrouper(newNamer(n), false, true, false, 0, false, false)
	for i, tc := range tests {
		got := rungroup(t, gr, procInfoIter(tc.proc))
		if diff := cmp.Diff(got, tc.want, opts); diff != "" {
			t.Errorf("%d: curgroups differs: (-got +want)\n%s", i, diff)
		}
	}
}

// piinfocg is like piinfo but also sets the process's cgroupv2 path.
func piinfocg(pid int, name string, cgroupV2Path string) IDInfo {
	pii := piinfo(pid, name, Counts{}, Memory{}, Filedesc{1, 400}, 1)
	pii.Static.CgroupV2Path = cgroupV2Path
	return pii
}

// TestGrouperCgroupV2Path verifies the group's cgroupv2 path aggregation and the
// skip-and-count conflict detection: procs sharing a path set it on the group;
// procs with differing paths flag a conflict so cgroup metrics get skipped.
func TestGrouperCgroupV2Path(t *testing.T) {
	tests := []struct {
		name         string
		procs        []IDInfo
		wantPath     string
		wantConflict bool
	}{
		{
			name:     "single proc sets path",
			procs:    []IDInfo{piinfocg(1, "g1", "/runtime.slice/nma.service")},
			wantPath: "/runtime.slice/nma.service",
		},
		{
			name: "same path across procs, no conflict",
			procs: []IDInfo{
				piinfocg(1, "g1", "/runtime.slice/nma.service"),
				piinfocg(2, "g1", "/runtime.slice/nma.service"),
			},
			wantPath: "/runtime.slice/nma.service",
		},
		{
			name: "differing paths flag a conflict",
			procs: []IDInfo{
				piinfocg(1, "g1", "/runtime.slice/nma.service"),
				piinfocg(2, "g1", "/runtime.slice/other.service"),
			},
			wantConflict: true,
		},
		{
			name:     "all-empty paths: no path, no conflict",
			procs:    []IDInfo{piinfocg(1, "g1", ""), piinfocg(2, "g1", "")},
			wantPath: "",
		},
		{
			name: "mixed empty then non-empty flags a conflict",
			procs: []IDInfo{
				piinfocg(1, "g1", ""),
				piinfocg(2, "g1", "/runtime.slice/nma.service"),
			},
			wantConflict: true,
		},
		{
			name: "mixed non-empty then empty flags a conflict",
			procs: []IDInfo{
				piinfocg(1, "g1", "/runtime.slice/nma.service"),
				piinfocg(2, "g1", ""),
			},
			wantConflict: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gr := NewGrouper(newNamer("g1"), false, false, false, 0, false, false)
			got := rungroup(t, gr, procInfoIter(tc.procs...))
			g := got["g1"]
			if g.CgroupV2Conflict != tc.wantConflict {
				t.Errorf("CgroupV2Conflict = %v, want %v", g.CgroupV2Conflict, tc.wantConflict)
			}
			// When a conflict is flagged the path is indeterminate (first-seen);
			// only assert the path when we don't expect a conflict.
			if !tc.wantConflict && g.CgroupV2Path != tc.wantPath {
				t.Errorf("CgroupV2Path = %q, want %q", g.CgroupV2Path, tc.wantPath)
			}
		})
	}
}
