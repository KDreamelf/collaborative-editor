package server

import (
	"context"
	"log"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// articleStore 进程内文章落盘；mongoStore 实现。测试可注入假实现。
type articleStore interface {
	listIDs(ctx context.Context) ([]string, error)
	load(ctx context.Context, idHex string) (*document.Doc, error)
	save(ctx context.Context, v document.View) error
}

type mongoStore struct {
	client *mongo.Client
	db     *mongo.Database
}

func connectMongo(uri, dbName string) *mongoStore {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		log.Printf("mongo connect: %v", err)
		return nil
	}
	if err := client.Ping(ctx, nil); err != nil {
		log.Printf("mongo ping: %v", err)
		_ = client.Disconnect(ctx)
		return nil
	}
	return &mongoStore{client: client, db: client.Database(dbName)}
}

func (s *mongoStore) listIDs(ctx context.Context) ([]string, error) {
	names, err := s.db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, err := model.ParseID(name); err != nil || name == "" {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func (s *mongoStore) load(ctx context.Context, idHex string) (*document.Doc, error) {
	cur, err := s.db.Collection(idHex).Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var raws []bson.Raw
	for cur.Next(ctx) {
		raws = append(raws, append(bson.Raw(nil), cur.Current...))
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	if len(raws) == 0 {
		return nil, mongo.ErrNoDocuments
	}
	article, lines, disputes, err := model.SplitBSON(raws)
	if err != nil {
		return nil, err
	}
	return document.Load(article, lines, disputes)
}

func (s *mongoStore) save(ctx context.Context, v document.View) error {
	idHex := v.Article.ID.Hex()
	col := s.db.Collection(idHex)
	docs := make([]any, 0, 1+len(v.Lines)+len(v.Disputes))
	docs = append(docs, v.Article)
	for i := range v.Lines {
		docs = append(docs, v.Lines[i])
	}
	for i := range v.Disputes {
		docs = append(docs, v.Disputes[i])
	}
	ids := make(bson.A, 0, len(docs))
	models := make([]mongo.WriteModel, 0, len(docs))
	for _, item := range docs {
		id, err := bsonID(item)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		models = append(models, mongo.NewReplaceOneModel().
			SetFilter(bson.M{"_id": id}).
			SetReplacement(item).
			SetUpsert(true))
	}
	if len(models) > 0 {
		if _, err := col.BulkWrite(ctx, models); err != nil {
			return err
		}
	}
	_, err := col.DeleteMany(ctx, bson.M{"_id": bson.M{"$nin": ids}})
	return err
}

func bsonID(item any) (model.ID, error) {
	switch v := item.(type) {
	case model.Article:
		return v.ID, nil
	case model.Line:
		return v.ID, nil
	case model.Dispute:
		return v.ID, nil
	default:
		return model.ID{}, errString("写库时认不出文档")
	}
}
