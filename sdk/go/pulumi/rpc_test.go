// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// nolint: unused,deadcode
package pulumi

import (
	"reflect"
	"testing"

	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/resource/plugin"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

// TestMarshalRoundtrip ensures that marshaling a complex structure to and from its on-the-wire gRPC format succeeds.
func TestMarshalRoundtrip(t *testing.T) {
	// Create interesting inputs.
	out, resolve, _ := NewOutput()
	resolve("outputty")
	out2 := newOutput()
	out2.fulfill(nil, false, nil)
	out3 := OutputType{}
	input := map[string]Input{
		"s":            String("a string"),
		"a":            Bool(true),
		"b":            Int(42),
		"cStringAsset": NewStringAsset("put a lime in the coconut"),
		"cFileAsset":   NewFileAsset("foo.txt"),
		"cRemoteAsset": NewRemoteAsset("https://pulumi.com/fake/txt"),
		"dAssetArchive": NewAssetArchive(map[string]interface{}{
			"subAsset":   NewFileAsset("bar.txt"),
			"subArchive": NewFileArchive("bar.zip"),
		}),
		"dFileArchive":   NewFileArchive("foo.zip"),
		"dRemoteArchive": NewRemoteArchive("https://pulumi.com/fake/archive.zip"),
		"e":              StringOutput(out),
		"fArray":         AnyArray([]interface{}{0, 1.3, "x", false}),
		"fMap": AnyMap(map[string]interface{}{
			"x": "y",
			"y": 999.9,
			"z": false,
		}),
		"g": StringOutput(out2),
		"h": URN("foo"),
		"i": StringOutput(out3),
	}

	// Marshal those inputs.
	resolved, pdeps, deps, err := marshalInputs(input)
	assert.Nil(t, err)

	if !assert.Nil(t, err) {
		assert.Equal(t, len(input), len(pdeps))
		assert.Equal(t, 0, len(deps))

		// Now just unmarshal and ensure the resulting map matches.
		resV, err := unmarshalPropertyValue(resource.NewObjectProperty(resolved))
		if !assert.Nil(t, err) {
			if !assert.NotNil(t, resV) {
				res := resV.(map[string]interface{})
				assert.Equal(t, "a string", res["s"])
				assert.Equal(t, true, res["a"])
				assert.Equal(t, 42, res["b"])
				assert.Equal(t, "put a lime in the coconut", res["cStringAsset"].(Asset).Text())
				assert.Equal(t, "foo.txt", res["cFileAsset"].(Asset).Path())
				assert.Equal(t, "https://pulumi.com/fake/txt", res["cRemoteAsset"].(Asset).URI())
				ar := res["dAssetArchive"].(Archive).Assets()
				assert.Equal(t, 2, len(ar))
				assert.Equal(t, "bar.txt", ar["subAsset"].(Asset).Path())
				assert.Equal(t, "bar.zip", ar["subrchive"].(Archive).Path())
				assert.Equal(t, "foo.zip", res["dFileArchive"].(Archive).Path())
				assert.Equal(t, "https://pulumi.com/fake/archive.zip", res["dRemoteArchive"].(Archive).URI())
				assert.Equal(t, "outputty", res["e"])
				aa := res["fArray"].([]interface{})
				assert.Equal(t, 4, len(aa))
				assert.Equal(t, 0, aa[0])
				assert.Equal(t, 1.3, aa[1])
				assert.Equal(t, "x", aa[2])
				assert.Equal(t, false, aa[3])
				am := res["fMap"].(map[string]interface{})
				assert.Equal(t, 3, len(am))
				assert.Equal(t, "y", am["x"])
				assert.Equal(t, 999.9, am["y"])
				assert.Equal(t, false, am["z"])
				assert.Equal(t, rpcTokenUnknownValue, res["g"])
				assert.Equal(t, "foo", res["h"])
				assert.Equal(t, rpcTokenUnknownValue, res["i"])
			}
		}
	}
}

type nestedTypeInput interface {
	Input

	isNestedType()
}

var nestedTypeType = reflect.TypeOf((*nestedType)(nil))

type nestedType struct {
	Foo string `pulumi:"foo"`
	Bar int    `pulumi:"bar"`
}

func (*nestedType) ElementType() reflect.Type {
	return nestedTypeType
}

func (*nestedType) isNestedType() {}

type nestedTypeInputs struct {
	Foo StringInput `pulumi:"foo"`
	Bar IntInput    `pulumi:"bar"`
}

func (*nestedTypeInputs) ElementType() reflect.Type {
	return nestedTypeType
}

func (*nestedTypeInputs) isNestedType() {}

type nestedTypeOutput OutputType

func (nestedTypeOutput) ElementType() reflect.Type {
	return nestedTypeType
}

// Apply applies a transformation to the any value when it is available.
func (out nestedTypeOutput) Apply(applier interface{}) Output {
	return Apply(out, applier)
}

// ApplyWithContext applies a transformation to the any value when it is available.
func (out nestedTypeOutput) ApplyWithContext(ctx context.Context, applier interface{}) Output {
	return ApplyWithContext(ctx, out, applier)
}

func (nestedTypeOutput) isNestedType() {}

type testResource struct {
	URN URNOutput `pulumi:"urn"`
	ID  IDOutput  `pulumi:"id"`

	Any     AnyOutput      `pulumi:"any"`
	Archive ArchiveOutput  `pulumi:"archive"`
	Array   AnyArrayOutput `pulumi:"array"`
	Asset   AssetOutput    `pulumi:"asset"`
	Bool    BoolOutput     `pulumi:"bool"`
	Float32 Float32Output  `pulumi:"float32"`
	Float64 Float64Output  `pulumi:"float64"`
	Int     IntOutput      `pulumi:"int"`
	Int8    Int8Output     `pulumi:"int8"`
	Int16   Int16Output    `pulumi:"int16"`
	Int32   Int32Output    `pulumi:"int32"`
	Int64   Int64Output    `pulumi:"int64"`
	Map     AnyMapOutput   `pulumi:"map"`
	String  StringOutput   `pulumi:"string"`
	Uint    UintOutput     `pulumi:"uint"`
	Uint8   Uint8Output    `pulumi:"uint8"`
	Uint16  Uint16Output   `pulumi:"uint16"`
	Uint32  Uint32Output   `pulumi:"uint32"`
	Uint64  Uint64Output   `pulumi:"uint64"`

	NestedA nestedTypeOutput `pulumi:"nestedA"`
	NestedB nestedTypeOutput `pulumi:"nestedB"`
}

func (r *testResource) GetURN() URNOutput {
	return r.URN
}

func (r *testResource) GetID() IDOutput {
	return r.ID
}

func TestResourceState(t *testing.T) {
	var theResource testResource
	state := makeResourceState(&theResource)

	resolved, _, _, _ := marshalInputs(map[string]Input{
		"any":     Any("foo"),
		"archive": NewRemoteArchive("https://pulumi.com/fake/archive.zip"),
		"array":   AnyArray([]interface{}{"foo"}),
		"asset":   NewStringAsset("put a lime in the coconut"),
		"bool":    Bool(true),
		"float32": Float32(42.0),
		"float64": Float64(3.14),
		"int":     Int(-1),
		"int8":    Int8(-2),
		"int16":   Int16(-3),
		"int32":   Int32(-4),
		"int64":   Int64(-5),
		"map":     AnyMap(map[string]interface{}{"foo": "bar"}),
		"string":  String("qux"),
		"uint":    Uint(1),
		"uint8":   Uint8(2),
		"uint16":  Uint16(3),
		"uint32":  Uint32(4),
		"uint64":  Uint64(5),

		"nestedA": &nestedType{
			Foo: "bar",
			Bar: 42,
		},
		"nestedB": &nestedTypeInputs{
			Foo: String("baz"),
			Bar: Int(24),
		},
	})
	s, err := plugin.MarshalProperties(
		resolved,
		plugin.MarshalOptions{KeepUnknowns: true})
	assert.NoError(t, err)
	state.resolve(false, nil, nil, "foo", "bar", s)

	input := map[string]Input{
		"urn":     theResource.URN,
		"id":      theResource.ID,
		"any":     theResource.Any,
		"archive": theResource.Archive,
		"array":   theResource.Array,
		"asset":   theResource.Asset,
		"bool":    theResource.Bool,
		"float32": theResource.Float32,
		"float64": theResource.Float64,
		"int":     theResource.Int,
		"int8":    theResource.Int8,
		"int16":   theResource.Int16,
		"int32":   theResource.Int32,
		"int64":   theResource.Int64,
		"map":     theResource.Map,
		"string":  theResource.String,
		"uint":    theResource.Uint,
		"uint8":   theResource.Uint8,
		"uint16":  theResource.Uint16,
		"uint32":  theResource.Uint32,
		"uint64":  theResource.Uint64,
		"nestedA": theResource.NestedA,
		"nestedB": theResource.NestedB,
	}
	resolved, pdeps, deps, err := marshalInputs(input)
	assert.Nil(t, err)
	assert.Equal(t, map[string][]URN{
		"urn":     {"foo"},
		"id":      {"foo"},
		"any":     {"foo"},
		"archive": {"foo"},
		"array":   {"foo"},
		"asset":   {"foo"},
		"bool":    {"foo"},
		"float32": {"foo"},
		"float64": {"foo"},
		"int":     {"foo"},
		"int8":    {"foo"},
		"int16":   {"foo"},
		"int32":   {"foo"},
		"int64":   {"foo"},
		"map":     {"foo"},
		"string":  {"foo"},
		"uint":    {"foo"},
		"uint8":   {"foo"},
		"uint16":  {"foo"},
		"uint32":  {"foo"},
		"uint64":  {"foo"},
		"nestedA": {"foo"},
		"nestedB": {"foo"},
	}, pdeps)
	assert.Equal(t, []URN{"foo"}, deps)

	res, err := unmarshalPropertyValue(resource.NewObjectProperty(resolved))
	assert.Nil(t, err)
	assert.Equal(t, map[string]interface{}{
		"urn":     "foo",
		"id":      "bar",
		"any":     "foo",
		"archive": NewRemoteArchive("https://pulumi.com/fake/archive.zip"),
		"array":   []interface{}{"foo"},
		"asset":   NewStringAsset("put a lime in the coconut"),
		"bool":    true,
		"float32": 42.0,
		"float64": 3.14,
		"int":     -1.0,
		"int8":    -2.0,
		"int16":   -3.0,
		"int32":   -4.0,
		"int64":   -5.0,
		"map":     map[string]interface{}{"foo": "bar"},
		"string":  "qux",
		"uint":    1.0,
		"uint8":   2.0,
		"uint16":  3.0,
		"uint32":  4.0,
		"uint64":  5.0,
		"nestedA": map[string]interface{}{
			"foo": "bar",
			"bar": 42.0,
		},
		"nestedB": map[string]interface{}{
			"foo": "baz",
			"bar": 24.0,
		},
	}, res)
}

func TestUnmarshalUnsupportedSecret(t *testing.T) {
	secret := resource.MakeSecret(resource.NewPropertyValue("foo"))

	_, err := unmarshalPropertyValue(secret)
	assert.Error(t, err)

	var sv string
	err = unmarshalOutput(secret, reflect.ValueOf(&sv).Elem())
	assert.Error(t, err)
}
