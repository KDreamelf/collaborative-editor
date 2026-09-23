package model

// 一篇文章一个集合，里面三种文档。
// BSON 字段用中文，和库里的原始数据一致。JSON 用英文，给页面和管理后台。

const (
	ActionEdit         = "改这行"
	ActionInsert       = "插在后面"
	ActionInsertBefore = "插在前面"
	ActionDelete       = "删这行"
)

type Article struct {
	ID    ID     `bson:"_id" json:"id"`
	Title string `bson:"标题" json:"title"`
}

// Line 是正式行。前后指针只串正式行。零值的前、后不写进库，首行因此没有「前」。
// 插入来源、最近编辑、编辑来源是已入链主张的可恢复出处；缺省表示旧数据或尚无来源。
type Line struct {
	ID           ID             `bson:"_id" json:"id"`
	Prev         ID             `bson:"前,omitempty" json:"prev"`
	Next         ID             `bson:"后,omitempty" json:"next"`
	Content      string         `bson:"内容" json:"content"`
	InsertOrigin *InsertOrigin  `bson:"插入来源,omitempty" json:"insertOrigin,omitempty"`
	RecentEdit   *RecentEdit    `bson:"最近编辑,omitempty" json:"recentEdit,omitempty"`
	EditOrigin   *EditOrigin    `bson:"编辑来源,omitempty" json:"editOrigin,omitempty"`
}

// InsertOrigin 挂在插入段首行：谁插入、锚在哪、方向、原边界、段内行、最初整段。
// Action 空或「插在后面」= 锚后插入（看 OldNext）；「插在前面」= 锚前插入（看 OldPrev）。
type InsertOrigin struct {
	Person  string   `bson:"插入者" json:"inserter"`
	Anchor  ID       `bson:"锚点" json:"anchor"`
	Action  string   `bson:"做法,omitempty" json:"action,omitempty"`
	OldNext ID       `bson:"原后继,omitempty" json:"oldNext,omitempty"`
	OldPrev ID       `bson:"原前驱,omitempty" json:"oldPrev,omitempty"`
	LineIDs []ID     `bson:"段内行" json:"lineIDs"`
	Content []string `bson:"最初内容" json:"originalContent"`
}

// RecentEdit 挂在普通单行编辑过的正式行。编辑前为空串时也要写出「编辑前」字段。
type RecentEdit struct {
	Person string `bson:"编辑者" json:"editor"`
	Before string `bson:"编辑前" json:"before"`
}

// EditOrigin 挂在跨度替换后的首行：谁改、原跨度、原正文、原后继、结果段内行、替换内容。
// 普通单行仍用 RecentEdit；跨多条正式行的整段主张用本字段，Load 可恢复 live。
type EditOrigin struct {
	Person    string   `bson:"编辑者" json:"editor"`
	BaseIDs   []ID     `bson:"基准行" json:"baseIDs"`
	BaseTexts []string `bson:"基准正文" json:"baseTexts"`
	OldNext   ID       `bson:"原后继,omitempty" json:"oldNext"`
	LineIDs   []ID     `bson:"段内行" json:"lineIDs"`
	Content   []string `bson:"替换内容" json:"content"`
}

// PendingConfirm 是挂在被追随方争议上的待确认追随。随争议文档入 Mongo，重启可恢复。
type PendingConfirm struct {
	From     string `bson:"发起人" json:"from"`
	To       string `bson:"目标" json:"to"`
	ClientTs int64  `bson:"客户端时戳" json:"clientTs"`
}

// Dispute 不进链。同一条正式行上，每人一份，真实行相同。
// 内容是一组字符串：改一行就是一项，一次粘贴的多行都在这一份里。
// 「插在前面」真实行=下方原行；Content 仅插入段。
// BaseIDs 非空时表示跨度整段「改这行」：追随收口要按原跨度替换；旧单行争议该字段为空。
type Dispute struct {
	ID        ID               `bson:"_id" json:"id"`
	RealLine  ID               `bson:"真实行" json:"realLine"`
	Action    string           `bson:"做法" json:"action"`
	Person    string           `bson:"人" json:"person"`
	Content   []string         `bson:"内容" json:"content"`
	BaseIDs   []ID             `bson:"基准行,omitempty" json:"baseIDs,omitempty"`
	Followers []string         `bson:"追随者" json:"followers"`
	Pending   []PendingConfirm `bson:"待确认" json:"pendingConfirm"`
}

// InsertAction 归一插入做法：空视为「插在后面」。
func InsertAction(action string) string {
	if action == "" || action == ActionInsert {
		return ActionInsert
	}
	return action
}

// IsInsertAction 是否插入类做法（后面/前面）。
func IsInsertAction(action string) bool {
	return action == ActionInsert || action == ActionInsertBefore
}
