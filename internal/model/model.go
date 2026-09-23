package model

// 一篇文章一个集合，里面三种文档。
// BSON 字段用中文，和库里的原始数据一致。JSON 用英文，给页面和管理后台。

const (
	ActionEdit   = "改这行"
	ActionInsert = "插在后面"
)

type Article struct {
	ID    ID     `bson:"_id" json:"id"`
	Title string `bson:"标题" json:"title"`
}

// Line 是正式行。前后指针只串正式行。零值的前、后不写进库，首行因此没有「前」。
type Line struct {
	ID      ID     `bson:"_id" json:"id"`
	Prev    ID     `bson:"前,omitempty" json:"prev"`
	Next    ID     `bson:"后,omitempty" json:"next"`
	Content string `bson:"内容" json:"content"`
}

// Dispute 不进链。同一条正式行上，每人一份，真实行相同。
// 内容是一组字符串：改一行就是一项，一次粘贴的多行都在这一份里。
type Dispute struct {
	ID        ID       `bson:"_id" json:"id"`
	RealLine  ID       `bson:"真实行" json:"realLine"`
	Action    string   `bson:"做法" json:"action"`
	Person    string   `bson:"人" json:"person"`
	Content   []string `bson:"内容" json:"content"`
	Followers []string `bson:"追随者" json:"followers"`
}
