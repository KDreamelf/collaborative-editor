package model

import (
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
)

// SplitBSON 把一篇文章的集合拆开。
// 有「真实行」是争议，有「标题」是文章，有「内容」是正式行。
// 文章和争议也没有「前」，不能拿这个字段当首行。
func SplitBSON(docs []bson.Raw) (Article, []Line, []Dispute, error) {
	var article Article
	var lines []Line
	var disputes []Dispute
	var seenArticle bool
	for _, raw := range docs {
		switch {
		case hasKey(raw, "真实行"):
			var d Dispute
			if err := bson.Unmarshal(raw, &d); err != nil {
				return Article{}, nil, nil, err
			}
			if d.Followers == nil {
				d.Followers = []string{}
			}
			disputes = append(disputes, d)
		case hasKey(raw, "标题"):
			if seenArticle {
				return Article{}, nil, nil, fmt.Errorf("集合里有两篇文档")
			}
			if err := bson.Unmarshal(raw, &article); err != nil {
				return Article{}, nil, nil, err
			}
			seenArticle = true
		case hasKey(raw, "内容"):
			var line Line
			if err := bson.Unmarshal(raw, &line); err != nil {
				return Article{}, nil, nil, err
			}
			lines = append(lines, line)
		default:
			return Article{}, nil, nil, fmt.Errorf("认不出的文档")
		}
	}
	if !seenArticle {
		return Article{}, nil, nil, fmt.Errorf("集合里没有文章文档")
	}
	return article, lines, disputes, nil
}

func hasKey(raw bson.Raw, key string) bool {
	_, err := raw.LookupErr(key)
	return err == nil
}
