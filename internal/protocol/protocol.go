package protocol

import "github.com/KDreamelf/collaborative-editor/internal/document"

// 页面和服务器之间的消息。主张、追随走这条转发，不走打洞。
// 光标现在也走转发。打洞以后再换，消息形状不用动。

const (
	TypeJoin         = "join"
	TypeBatch        = "batch"
	TypeSubmit       = "submit"
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
)

type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Cursor struct {
	PersonID  string `json:"personId"`
	Name      string `json:"name"`
	LineID    string `json:"lineId"`
	DisputeID string `json:"disputeId,omitempty"`
	Offset    int    `json:"offset"`
	SelEnd    int    `json:"selEnd"`
}

type Join struct {
	Type      string `json:"type"`
	ArticleID string `json:"articleId"`
	PersonID  string `json:"personId"`
	Name      string `json:"name"`
}

// Submit 是一份主张。
type Submit struct {
	Type     string   `json:"type"`
	PersonID string   `json:"personId"`
	LineID   string   `json:"lineId"`
	Action   string   `json:"action"`
	Content  []string `json:"content"`
	ClientTs int64    `json:"clientTs"`
}

// Op 是队列里的一步。批量只是一起送，不把几步并成一份主张。
type Op struct {
	Kind   string  `json:"kind"` // submit、delete、merge
	Submit *Submit `json:"submit,omitempty"`
	Delete *Delete `json:"delete,omitempty"`
	Merge  *Merge  `json:"merge,omitempty"`
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
