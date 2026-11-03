package main

import (
	"reflect"
	"testing"
)

func TestWorkStateFold(t *testing.T) {
	type step struct {
		project, list string
		row           map[string]any
	}
	tests := []struct {
		name    string
		steps   []step
		project string
		want    map[string][]map[string]any
	}{
		{
			name:    "empty",
			project: "p",
			want:    map[string][]map[string]any{},
		},
		{
			name: "item field updated twice: latest wins",
			steps: []step{
				{"p", "todo", map[string]any{"task": "a", "status": "open", "note": "first"}},
				{"p", "todo", map[string]any{"task": "a", "note": "second"}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"todo": {{"task": "a", "status": "open", "note": "second"}},
			},
		},
		{
			name: "null clears an item field",
			steps: []step{
				{"p", "todo", map[string]any{"task": "a", "status": "open", "note": "x"}},
				{"p", "todo", map[string]any{"task": "a", "note": nil}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"todo": {{"task": "a", "status": "open"}},
			},
		},
		{
			name: "status done closes only that item; items keep first-appearance order",
			steps: []step{
				{"p", "todo", map[string]any{"task": "a", "status": "open"}},
				{"p", "todo", map[string]any{"task": "b", "status": "open"}},
				{"p", "todo", map[string]any{"task": "c", "status": "open"}},
				{"p", "todo", map[string]any{"task": "a", "status": "done"}},
				{"p", "todo", map[string]any{"task": "c", "status": "dropped"}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"todo": {{"task": "b", "status": "open"}},
			},
		},
		{
			name: "closed item reopened by a later row starts fresh",
			steps: []step{
				{"p", "todo", map[string]any{"task": "a", "status": "open", "note": "old"}},
				{"p", "todo", map[string]any{"task": "a", "status": "done"}},
				{"p", "todo", map[string]any{"task": "a", "status": "open"}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"todo": {{"task": "a", "status": "open"}},
			},
		},
		{
			name: "list-level row without task: latest key wins, null clears, listed before items",
			steps: []step{
				{"p", "plan", map[string]any{"task": "t1", "status": "open"}},
				{"p", "plan", map[string]any{"goal": "g1", "owner": "me"}},
				{"p", "plan", map[string]any{"goal": "g2", "owner": nil}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"plan": {{"goal": "g2"}, {"task": "t1", "status": "open"}},
			},
		},
		{
			name: "list-level status done closes the whole list including items",
			steps: []step{
				{"p", "plan", map[string]any{"task": "t1", "status": "open"}},
				{"p", "plan", map[string]any{"goal": "g"}},
				{"p", "plan", map[string]any{"status": "done"}},
				{"p", "other", map[string]any{"task": "x"}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"other": {{"task": "x"}},
			},
		},
		{
			name: "facts keep durable entries; null deletes a key; item-style status never closes",
			steps: []step{
				{"p", "facts", map[string]any{"db": "duckdb", "region": "us"}},
				{"p", "facts", map[string]any{"region": nil, "status": "done"}},
				{"p", "facts", map[string]any{"db": "duckdb2"}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"facts": {{"db": "duckdb2", "status": "done"}},
			},
		},
		{
			name: "user-facts are visible from every project",
			steps: []step{
				{"p1", "user-facts", map[string]any{"editor": "vim"}},
				{"p2", "user-facts", map[string]any{"shell": "zsh"}},
			},
			project: "p3",
			want: map[string][]map[string]any{
				"user-facts": {{"editor": "vim", "shell": "zsh"}},
			},
		},
		{
			name: "task key in facts and user-facts is a plain fact key, never an item",
			steps: []step{
				{"p", "facts", map[string]any{"task": "write docs"}},
				{"p", "facts", map[string]any{"task": "x", "status": "done"}},
				{"p", "user-facts", map[string]any{"task": "y", "status": "dropped"}},
			},
			project: "p",
			want: map[string][]map[string]any{
				"facts":      {{"task": "x", "status": "done"}},
				"user-facts": {{"task": "y", "status": "dropped"}},
			},
		},
		{
			name: "other projects' lists are excluded",
			steps: []step{
				{"p1", "todo", map[string]any{"task": "a"}},
				{"p2", "todo", map[string]any{"task": "b"}},
				{"p1", "facts", map[string]any{"k": "v1"}},
				{"p2", "facts", map[string]any{"k": "v2"}},
			},
			project: "p1",
			want: map[string][]map[string]any{
				"todo":  {{"task": "a"}},
				"facts": {{"k": "v1"}},
			},
		},
		{
			name: "facts with every key deleted are omitted",
			steps: []step{
				{"p", "facts", map[string]any{"k": "v"}},
				{"p", "facts", map[string]any{"k": nil}},
			},
			project: "p",
			want:    map[string][]map[string]any{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _ := openTestMemd(t)
			for _, s := range tt.steps {
				if err := workStateAppend(db, s.project, s.list, s.row); err != nil {
					t.Fatalf("workStateAppend: %v", err)
				}
			}
			got, err := workStateFold(db, tt.project)
			if err != nil {
				t.Fatalf("workStateFold: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("fold = %#v\nwant   %#v", got, tt.want)
			}
		})
	}
}

func TestWorkStateAppendRejectsEmptyList(t *testing.T) {
	db, _ := openTestMemd(t)
	if err := workStateAppend(db, "p", "", map[string]any{"k": "v"}); err == nil {
		t.Fatal("want error for empty list name")
	}
}
