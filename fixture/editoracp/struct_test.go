package main

import (
	"slices"
	"testing"
)

func TestOneCursor(t *testing.T) {
	e := newEditor()
	e.order = []string{"a", "b", "c"}
	e.formal = map[string]string{"a": "甲", "b": "", "c": "丙"}
	e.local = map[string]string{"a": "甲", "b": "", "c": "丙"}
	e.cursor = "a"
	e.cursor = "c"
	if e.cursor != "c" {
		t.Fatal("光标应只停在最后一处")
	}
	e.focusReadEnd()
	if e.cursor != "c" {
		t.Fatalf("读完应停在末行: %s", e.cursor)
	}
	e.cursor = "b"
	e.applyDelete("b")
	if e.cursor != "c" {
		t.Fatalf("删掉光标所在空行后应落到下一行: %s", e.cursor)
	}
	e.cursor = "a"
	e.applyMerge("c")
	if e.cursor != "a" || e.formal["a"] != "甲丙" {
		t.Fatalf("合并别的行不挪光标: cursor=%s formal=%q", e.cursor, e.formal["a"])
	}
}

func TestSpanText(t *testing.T) {
	one := spanText("span 1 3 只留一句")
	if !slices.Equal(one, []string{"只留一句"}) {
		t.Fatalf("单行: %#v", one)
	}
	two := spanText("span 2 4\n甲\n乙")
	if !slices.Equal(two, []string{"甲", "乙"}) {
		t.Fatalf("换行: %#v", two)
	}
}

func TestStructuralApply(t *testing.T) {
	e := newEditor()
	e.order = []string{"a", "b", "c"}
	e.formal = map[string]string{"a": "甲", "b": "", "c": "丙"}
	e.local = map[string]string{"a": "甲", "b": "", "c": "丙"}
	e.applyDelete("a")
	if len(e.order) != 3 {
		t.Fatal("非空行不能删")
	}
	e.applyDelete("b")
	if !slices.Equal(e.order, []string{"a", "c"}) {
		t.Fatalf("删空行: %#v", e.order)
	}
	e.applyMerge("c")
	if !slices.Equal(e.order, []string{"a"}) || e.formal["a"] != "甲丙" || e.local["a"] != "甲丙" {
		t.Fatalf("向上合并: order=%#v formal=%q", e.order, e.formal["a"])
	}

	e.order = []string{"a", "b", "c", "d"}
	e.formal = map[string]string{"a": "A", "b": "B", "c": "C", "d": "D"}
	e.local = map[string]string{"a": "A", "b": "B", "c": "C", "d": "D"}
	e.applySpan([]string{"b", "c"}, []string{"X", "Y"}, []string{"n"})
	if !slices.Equal(e.order, []string{"a", "b", "n", "d"}) {
		t.Fatalf("整段替换顺序: %#v", e.order)
	}
	if e.formal["b"] != "X" || e.formal["n"] != "Y" || e.formal["d"] != "D" {
		t.Fatalf("正文: %#v", e.formal)
	}
}
