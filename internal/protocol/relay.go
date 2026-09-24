package protocol

import (
	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
)

// DisputeCC 是客户端主张抄送：目标人 + 现有 model.Dispute，不另造文档类型。
type DisputeCC struct {
	TargetPersonID string        `json:"targetPersonId"`
	Claim          model.Dispute `json:"claim"`
}

// RelayEvent 是服务器→客户端的转发事件。
type RelayEvent struct {
	Type string `json:"type"`
	Op   Op     `json:"op"`
}

// Bootstrap 是入场包：当前正式行链 + 显式主张 + 在场信息。
type Bootstrap struct {
	Type     string          `json:"type"`
	Base     document.View   `json:"base"`
	Disputes []model.Dispute `json:"disputes"`
	YourLine string          `json:"yourLine,omitempty"`
	People   []Person        `json:"people"`
	Cursors  []Cursor        `json:"cursors"`
}
