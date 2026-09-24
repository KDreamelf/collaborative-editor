package protocol

import (
	"fmt"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
)

// 页面和服务器之间的消息。主张、追随走这条转发，不走打洞。
// 光标现在也走转发。打洞以后再换，消息形状不用动。

const (
	TypeJoin         = "join"
	TypeBatch        = "batch"
	TypeSubmit       = "submit"
	TypeSpanEdit     = "spanEdit"
	TypeSuspend      = "suspend"
	TypeFollow       = "follow"
	TypeFollowAnswer = "followAnswer"
	TypeCursor       = "cursor"
	TypeDelete       = "delete"
	TypeMerge        = "merge"
	TypeSnapshot     = "snapshot"
	TypeFollowAsk    = "followAsk"
	TypeFollowResult = "followResult"
	TypeCursors      = "cursors"
	TypeAck          = "ack"
	TypeError        = "error"
	TypeDisputeCC    = "disputeCC"
	TypeRelay        = "relay"
	TypeBootstrap    = "bootstrap"
)

type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Cursor struct {
	PersonID        string `json:"personId"`
	Name            string `json:"name"`
	LineID          string `json:"lineId"`
	DisputeID       string `json:"disputeId,omitempty"`
	Offset          int    `json:"offset"`
	SelEnd          int    `json:"selEnd"`
	PartIndex       int    `json:"partIndex,omitempty"`
	SelEndLineID    string `json:"selEndLineId,omitempty"`
	SelEndDisputeID string `json:"selEndDisputeId,omitempty"`
	SelEndPartIndex int    `json:"selEndPartIndex,omitempty"`
}

type Join struct {
	Type      string `json:"type"`
	ArticleID string `json:"articleId"`
	PersonID  string `json:"personId"`
	Name      string `json:"name"`
}

// Submit 是一份主张。
// AfterSeen：客户端最后已应用的服务端快照里，锚点当时的后继 ID。
// nil=字段缺失（兼容旧包）；非 nil 且空串=已知末尾。乐观本地改动不能写进这个基准。
// BeforeSeen：快照里该行当时的前驱 ID。「插在前面」用；nil=旧包；非 nil 且空串=已知文首。
// BaseContent：改这行时，快照里该行当时的正式内容。nil=旧包；非 nil（含空串）=已知基准。
// LineIDs：客户端预生的新正式行 ID。插在后面/前面时与 Content 等长；改这行多行粘贴时对应 Content[1:]。
// WholeClaim：false/缺省=普通正式行编辑；true=明确更新本人整份候选。
type Submit struct {
	Type        string   `json:"type"`
	PersonID    string   `json:"personId"`
	LineID      string   `json:"lineId"`
	Action      string   `json:"action"`
	Content     []string `json:"content"`
	ClientTs    int64    `json:"clientTs"`
	AfterSeen   *string  `json:"afterSeen,omitempty"`
	BeforeSeen  *string  `json:"beforeSeen,omitempty"`
	BaseContent *string  `json:"baseContent,omitempty"`
	LineIDs     []string `json:"lineIds,omitempty"`
	WholeClaim  bool     `json:"wholeClaim,omitempty"`
}

// SpanEdit 跨多条正式行的一次整段替换。独立 kind，旧服务端遇未知 kind 会拒绝，避免误当单行粘贴。
// AfterSeen 空串=已知文末。BaseIDs 至少 2；LineIDs 对应 Replacement[1:]。
type SpanEdit struct {
	Type        string   `json:"type"`
	PersonID    string   `json:"personId"`
	BaseIDs     []string `json:"baseIds"`
	BaseTexts   []string `json:"baseTexts"`
	AfterSeen   string   `json:"afterSeen"`
	Replacement []string `json:"replacement"`
	LineIDs     []string `json:"lineIds,omitempty"`
	ClientTs    int64    `json:"clientTs"`
}

// Op 是队列里的一步。批量只是一起送，不把几步并成一份主张。
// ID 由客户端生成，服务端按它去重，重连重发不会再执行一遍。
type Op struct {
	ID           string        `json:"id,omitempty"`
	Kind         string        `json:"kind"` // submit、spanEdit、delete、merge、follow、followAnswer、suspend、disputeCC
	Submit       *Submit       `json:"submit,omitempty"`
	SpanEdit     *SpanEdit     `json:"spanEdit,omitempty"`
	Delete       *Delete       `json:"delete,omitempty"`
	Merge        *Merge        `json:"merge,omitempty"`
	Follow       *Follow       `json:"follow,omitempty"`
	FollowAnswer *FollowAnswer `json:"followAnswer,omitempty"`
	Suspend      *Suspend      `json:"suspend,omitempty"`
	DisputeCC    *DisputeCC    `json:"disputeCC,omitempty"`
}

// ParseSpanEditOpts 把协议包转成领域参数；空字段/非法 ID 与领域校验一致。
func ParseSpanEditOpts(s *SpanEdit) (document.SpanEditOpts, error) {
	if s == nil {
		return document.SpanEditOpts{}, fmt.Errorf("缺少 spanEdit")
	}
	if s.PersonID == "" {
		return document.SpanEditOpts{}, fmt.Errorf("缺少 personId")
	}
	if len(s.BaseIDs) < 2 || len(s.BaseIDs) != len(s.BaseTexts) {
		return document.SpanEditOpts{}, document.ErrSpanBase
	}
	if len(s.Replacement) == 0 {
		return document.SpanEditOpts{}, document.ErrContent
	}
	want := 0
	if len(s.Replacement) > 1 {
		want = len(s.Replacement) - 1
	}
	if len(s.LineIDs) != want {
		return document.SpanEditOpts{}, document.ErrLineIDs
	}
	opts := document.SpanEditOpts{
		BaseTexts:   append([]string(nil), s.BaseTexts...),
		Replacement: append([]string(nil), s.Replacement...),
	}
	opts.BaseIDs = make([]model.ID, len(s.BaseIDs))
	seen := map[model.ID]bool{}
	for i, raw := range s.BaseIDs {
		if raw == "" {
			return document.SpanEditOpts{}, document.ErrSpanBase
		}
		id, err := model.ParseID(raw)
		if err != nil || id.IsZero() || seen[id] {
			return document.SpanEditOpts{}, document.ErrSpanBase
		}
		seen[id] = true
		opts.BaseIDs[i] = id
	}
	after, err := model.ParseID(s.AfterSeen)
	if err != nil {
		return document.SpanEditOpts{}, document.ErrSpanBase
	}
	opts.AfterSeen = after
	if want > 0 {
		opts.LineIDs = make([]model.ID, want)
		for i, raw := range s.LineIDs {
			if raw == "" {
				return document.SpanEditOpts{}, document.ErrLineIDs
			}
			id, err := model.ParseID(raw)
			if err != nil || id.IsZero() || seen[id] {
				return document.SpanEditOpts{}, document.ErrLineIDs
			}
			seen[id] = true
			opts.LineIDs[i] = id
		}
	}
	return opts, nil
}

type Batch struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
	Ops  []Op   `json:"ops"`
}

// Ack 表示这一批已经按顺序处理了 Applied 步。后面的步没有执行。
type Ack struct {
	Type    string `json:"type"`
	Seq     int64  `json:"seq"`
	Applied int    `json:"applied"`
	Message string `json:"message,omitempty"`
}

type Suspend struct {
	Type      string `json:"type"`
	PersonID  string `json:"personId"`
	LineID    string `json:"lineId"`
	Action    string `json:"action"`
	Suspended bool   `json:"suspended"`
}

type Follow struct {
	Type      string `json:"type"`
	PersonID  string `json:"personId"`
	DisputeID string `json:"disputeId"`
	ClientTs  int64  `json:"clientTs"`
}

type FollowAnswer struct {
	Type      string `json:"type"`
	PersonID  string `json:"personId"`
	FromID    string `json:"fromId"`
	DisputeID string `json:"disputeId"`
	Accept    bool   `json:"accept"`
}

type CursorMsg struct {
	Type   string `json:"type"`
	Cursor Cursor `json:"cursor"`
}

// Delete 是空行再退格。Merge 是行首退格。有没有别人还停在这行上，由服务端按光标判断。
type Delete struct {
	Type     string `json:"type"`
	PersonID string `json:"personId"`
	LineID   string `json:"lineId"`
}

type Merge struct {
	Type     string `json:"type"`
	PersonID string `json:"personId"`
	LineID   string `json:"lineId"`
}

type Snapshot struct {
	Type     string `json:"type"`
	YourLine string `json:"yourLine,omitempty"`
	document.View
	People  []Person `json:"people"`
	Cursors []Cursor `json:"cursors"`
}

// FollowAsk 发给被追随的人：对方要等他点头。
type FollowAsk struct {
	Type      string `json:"type"`
	FromID    string `json:"fromId"`
	FromName  string `json:"fromName"`
	DisputeID string `json:"disputeId"`
	ClientTs  int64  `json:"clientTs"`
}

// FollowResult 告诉发起的人：pending 还在等，applied 生效，lost 互追时自己更晚。
type FollowResult struct {
	Type      string `json:"type"`
	Status    string `json:"status"`
	DisputeID string `json:"disputeId,omitempty"`
}

type ErrMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
