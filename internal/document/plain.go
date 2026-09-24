package document

import (
	"errors"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

// ErrPerson 普通同步要求非空提交者。
var ErrPerson = errors.New("人不能为空")

// AppendPlainBlankIDs 尾部接给定 ID 的空正式行（系统入场空行）。
// 复用 cloneDoc/checkNewLineIDs/spliceBetween/ordered；失败零突变。
// 不写 InsertOrigin/live/insertHistory/Dispute，避免 InsertBelief 污染。
func (d *Doc) AppendPlainBlankIDs(ids []model.ID) error {
	if len(ids) == 0 {
		return nil
	}
	ids = append([]model.ID(nil), ids...)
	texts := make([]string, len(ids))
	seen := map[model.ID]bool{}
	for _, id := range ids {
		if id.IsZero() || seen[id] {
			return ErrLineIDs
		}
		seen[id] = true
	}
	if d.alreadySpliced(ids, texts) {
		lines, err := d.ordered()
		if err != nil {
			return err
		}
		if len(lines) >= len(ids) {
			atTail := true
			for i, id := range ids {
				if lines[len(lines)-len(ids)+i].ID != id {
					atTail = false
					break
				}
			}
			if atTail {
				return nil
			}
		}
		return ErrLineIDs
	}
	if err := d.checkNewLineIDs(ids); err != nil {
		return err
	}
	working := d.cloneDoc()
	lines, err := working.ordered()
	if err != nil {
		return err
	}
	tail := lines[len(lines)-1].ID
	if _, err := working.spliceBetween(tail, model.ID{}, texts, ids); err != nil {
		return err
	}
	if _, err := working.ordered(); err != nil {
		return err
	}
	*d = *working
	return nil
}

// ApplyPlainSubmit 供中立服务器机械更新正式行链。
// 不读 BaseContent/AfterSeen/live/cursors，不新建 Dispute，不碰 SubmitWith/openDispute/tryResolve。
// 已有显式 Disputes 原样保留。失败零突变。
// Insert/InsertBefore 与多行 Edit 必须带齐客户端预生 LineIDs；重发仅 alreadySpliced（行 ID 相连且正文相同）时幂等。
func (d *Doc) ApplyPlainSubmit(person string, line model.ID, action string, content []string, lineIDs []model.ID) error {
	if person == "" {
		return ErrPerson
	}
	if action != model.ActionEdit && !model.IsInsertAction(action) {
		return ErrAction
	}
	if _, err := d.line(line); err != nil {
		return err
	}
	if len(content) == 0 {
		return ErrContent
	}
	content = append([]string(nil), content...)
	lineIDs = append([]model.ID(nil), lineIDs...)
	if err := validatePlainLineIDs(action, content, lineIDs); err != nil {
		return err
	}
	if len(lineIDs) > 0 && d.alreadySpliced(lineIDs, plainSpliceTexts(action, content)) {
		return nil
	}
	if err := d.checkNewLineIDs(lineIDs); err != nil {
		return err
	}

	working := d.cloneDoc()
	if err := working.applyPlain(person, line, action, content, lineIDs); err != nil {
		return err
	}
	if _, err := working.ordered(); err != nil {
		return err
	}
	*d = *working
	return nil
}

func validatePlainLineIDs(action string, content []string, ids []model.ID) error {
	if err := validateLineIDs(action, content, ids); err != nil {
		return err
	}
	// 共享 validateLineIDs 仍允空（旧 Submit 路径自生 ID）；plain 机械同步必须客户端预生。
	if model.IsInsertAction(action) || (action == model.ActionEdit && len(content) > 1) {
		if len(ids) == 0 {
			return ErrLineIDs
		}
	}
	return nil
}

func plainSpliceTexts(action string, content []string) []string {
	if action == model.ActionEdit {
		return content[1:]
	}
	return content
}

func (d *Doc) applyPlain(person string, line model.ID, action string, content []string, lineIDs []model.ID) error {
	ln := d.lines[line]
	switch {
	case action == model.ActionEdit:
		ln.Content = content[0]
		if len(content) == 1 {
			return nil
		}
		_, err := d.spliceBetween(line, ln.Next, content[1:], lineIDs)
		return err
	case action == model.ActionInsertBefore:
		oldPrev := ln.Prev
		if _, err := d.spliceBetween(oldPrev, line, content, lineIDs); err != nil {
			return err
		}
		d.setInsertOrigin(lineIDs, person, line, action, oldPrev, model.ID{}, content)
		d.archiveLiveInsert(claimKey{line, model.ActionInsertBefore}, &liveClaim{
			person:  person,
			content: append([]string(nil), content...),
			oldPrev: oldPrev,
			spliced: true,
			ids:     append([]model.ID(nil), lineIDs...),
		})
		return nil
	default:
		oldNext := ln.Next
		if _, err := d.spliceBetween(line, oldNext, content, lineIDs); err != nil {
			return err
		}
		d.setInsertOrigin(lineIDs, person, line, action, model.ID{}, oldNext, content)
		d.archiveLiveInsert(claimKey{line, model.ActionInsert}, &liveClaim{
			person:  person,
			content: append([]string(nil), content...),
			oldNext: oldNext,
			spliced: true,
			ids:     append([]model.ID(nil), lineIDs...),
		})
		return nil
	}
}
