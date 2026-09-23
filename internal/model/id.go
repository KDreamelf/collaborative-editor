package model

import (
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ID 是 Go 里生成的 MongoDB ObjectID。零值表示空：正式行的首行没有「前」，末行没有「后」。
type ID primitive.ObjectID

func NewID() ID { return ID(primitive.NewObjectID()) }

func (id ID) IsZero() bool { return primitive.ObjectID(id).IsZero() }

func (id ID) Hex() string {
	if id.IsZero() {
		return ""
	}
	return primitive.ObjectID(id).Hex()
}

func ParseID(s string) (ID, error) {
	if s == "" {
		return ID{}, nil
	}
	oid, err := primitive.ObjectIDFromHex(s)
	if err != nil {
		return ID{}, err
	}
	return ID(oid), nil
}

func (id ID) MarshalJSON() ([]byte, error) {
	if id.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(primitive.ObjectID(id).Hex())
}

func (id *ID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := ParseID(s)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id ID) MarshalBSONValue() (bsontype.Type, []byte, error) {
	if id.IsZero() {
		return bsontype.Null, nil, nil
	}
	oid := primitive.ObjectID(id)
	raw := make([]byte, len(oid))
	copy(raw, oid[:])
	return bsontype.ObjectID, raw, nil
}

func (id *ID) UnmarshalBSONValue(t bsontype.Type, data []byte) error {
	if t == bsontype.Null {
		*id = ID{}
		return nil
	}
	if t != bsontype.ObjectID || len(data) != 12 {
		return fmt.Errorf("不是 ObjectID")
	}
	var oid primitive.ObjectID
	copy(oid[:], data)
	*id = ID(oid)
	return nil
}
