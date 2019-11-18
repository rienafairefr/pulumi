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

// nolint: lll
package pulumi

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func await(out Output) (interface{}, bool, error) {
	return out.await(context.Background())
}

func assertApplied(t *testing.T, out Output) {
	_, known, err := await(out)
	assert.True(t, known)
	assert.Nil(t, err)
}

func newIntOutput() IntOutput {
	return IntOutput{newOutputState(reflect.TypeOf(42))}
}

func TestBasicOutputs(t *testing.T) {
	// Just test basic resolve and reject functionality.
	{
		out, resolve, _ := NewOutput()
		go func() {
			resolve(42)
		}()
		v, known, err := await(out)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.NotNil(t, v)
		assert.Equal(t, 42, v.(int))
	}
	{
		out, _, reject := NewOutput()
		go func() {
			reject(errors.New("boom"))
		}()
		v, _, err := await(out)
		assert.NotNil(t, err)
		assert.Nil(t, v)
	}
}

func TestArrayOutputs(t *testing.T) {
	out := AnyArrayOutput{newOutputState(reflect.TypeOf([]interface{}{}))}
	go func() {
		out.resolve([]interface{}{nil, 0, "x"}, true)
	}()
	{
		assertApplied(t, out.Apply(func(arr []interface{}) (interface{}, error) {
			assert.NotNil(t, arr)
			if assert.Equal(t, 3, len(arr)) {
				assert.Equal(t, nil, arr[0])
				assert.Equal(t, 0, arr[1])
				assert.Equal(t, "x", arr[2])
			}
			return nil, nil
		}))
	}
}

func TestBoolOutputs(t *testing.T) {
	out := BoolOutput{newOutputState(reflect.TypeOf(false))}
	go func() {
		out.resolve(true, true)
	}()
	{
		assertApplied(t, out.Apply(func(v bool) (interface{}, error) {
			assert.True(t, v)
			return nil, nil
		}))
	}
}

func TestMapOutputs(t *testing.T) {
	out := AnyMapOutput{newOutputState(reflect.TypeOf(map[string]interface{}{}))}
	go func() {
		out.resolve(map[string]interface{}{
			"x": 1,
			"y": false,
			"z": "abc",
		}, true)
	}()
	{
		assertApplied(t, out.Apply(func(v map[string]interface{}) (interface{}, error) {
			assert.NotNil(t, v)
			assert.Equal(t, 1, v["x"])
			assert.Equal(t, false, v["y"])
			assert.Equal(t, "abc", v["z"])
			return nil, nil
		}))
	}
}

func TestNumberOutputs(t *testing.T) {
	out := Float64Output{newOutputState(reflect.TypeOf(float64(0)))}
	go func() {
		out.resolve(42.345, true)
	}()
	{
		assertApplied(t, out.Apply(func(v float64) (interface{}, error) {
			assert.Equal(t, 42.345, v)
			return nil, nil
		}))
	}
}

func TestStringOutputs(t *testing.T) {
	out := StringOutput{newOutputState(reflect.TypeOf(""))}
	go func() {
		out.resolve("a stringy output", true)
	}()
	{
		assertApplied(t, out.Apply(func(v string) (interface{}, error) {
			assert.Equal(t, "a stringy output", v)
			return nil, nil
		}))
	}
}

func TestResolveOutputToOutput(t *testing.T) {
	// Test that resolving an output to an output yields the value, not the output.
	{
		out, resolve, _ := NewOutput()
		go func() {
			other, resolveOther, _ := NewOutput()
			resolve(other)
			go func() { resolveOther(99) }()
		}()
		assertApplied(t, out.Apply(func(v interface{}) (interface{}, error) {
			assert.Equal(t, v, 99)
			return nil, nil
		}))
	}
	// Similarly, test that resolving an output to a rejected output yields an error.
	{
		out, resolve, _ := NewOutput()
		go func() {
			other, _, rejectOther := NewOutput()
			resolve(other)
			go func() { rejectOther(errors.New("boom")) }()
		}()
		v, _, err := await(out)
		assert.NotNil(t, err)
		assert.Nil(t, v)
	}
}

func TestOutputApply(t *testing.T) {
	// Test that resolved outputs lead to applies being run.
	{
		out := newIntOutput()
		go func() { out.resolve(42, true) }()
		var ranApp bool
		app := out.Apply(func(v int) (interface{}, error) {
			ranApp = true
			return v + 1, nil
		})
		v, known, err := await(app)
		assert.True(t, ranApp)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, v, 43)
	}
	// Test that resolved, but unknown outputs, skip the running of applies.
	{
		out := newIntOutput()
		go func() { out.resolve(42, false) }()
		var ranApp bool
		app := out.Apply(func(v int) (interface{}, error) {
			ranApp = true
			return v + 1, nil
		})
		_, known, err := await(app)
		assert.False(t, ranApp)
		assert.Nil(t, err)
		assert.False(t, known)
	}
	// Test that rejected outputs do not run the apply, and instead flow the error.
	{
		out := newIntOutput()
		go func() { out.reject(errors.New("boom")) }()
		var ranApp bool
		app := out.Apply(func(v int) (interface{}, error) {
			ranApp = true
			return v + 1, nil
		})
		v, _, err := await(app)
		assert.False(t, ranApp)
		assert.NotNil(t, err)
		assert.Nil(t, v)
	}
	// Test that an an apply that returns an output returns the resolution of that output, not the output itself.
	{
		out := newIntOutput()
		go func() { out.resolve(42, true) }()
		var ranApp bool
		app := out.Apply(func(v int) (interface{}, error) {
			other, resolveOther, _ := NewOutput()
			go func() { resolveOther(v + 1) }()
			ranApp = true
			return other, nil
		})
		v, known, err := await(app)
		assert.True(t, ranApp)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, v, 43)

		app = out.Apply(func(v int) (interface{}, error) {
			other, resolveOther, _ := NewOutput()
			go func() { resolveOther(v + 2) }()
			ranApp = true
			return other, nil
		})
		v, known, err = await(app)
		assert.True(t, ranApp)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, v, 44)
	}
	// Test that an an apply that reject an output returns the rejection of that output, not the output itself.
	{
		out := newIntOutput()
		go func() { out.resolve(42, true) }()
		var ranApp bool
		app := out.Apply(func(v int) (interface{}, error) {
			other, _, rejectOther := NewOutput()
			go func() { rejectOther(errors.New("boom")) }()
			ranApp = true
			return other, nil
		})
		v, _, err := await(app)
		assert.True(t, ranApp)
		assert.NotNil(t, err)
		assert.Nil(t, v)

		app = out.Apply(func(v int) (interface{}, error) {
			other, _, rejectOther := NewOutput()
			go func() { rejectOther(errors.New("boom")) }()
			ranApp = true
			return other, nil
		})
		v, _, err = await(app)
		assert.True(t, ranApp)
		assert.NotNil(t, err)
		assert.Nil(t, v)
	}
	// Test that applies return appropriate concrete implementations of Output based on the callback type
	{
		out := newIntOutput()
		go func() { out.resolve(42, true) }()

		var ok bool
{{range .Builtins}}
		_, ok = out.Apply(func(v int) {{.ElementType}} { return *new({{.ElementType}}) }).({{.Name}}Output)
		assert.True(t, ok)
{{end}}
	}
	// Test some chained applies.
	{
		type myStructType struct {
			foo int
			bar string
		}

		out := newIntOutput()
		go func() { out.resolve(42, true) }()

		out2 := StringOutput{newOutputState(reflect.TypeOf(""))}
		go func() { out2.resolve("hello", true) }()

		res := out.
			Apply(func(v int) myStructType {
				return myStructType{foo: v, bar: "qux,zed"}
			}).
			Apply(func(v interface{}) (string, error) {
				bar := v.(myStructType).bar
				if bar != "qux,zed" {
					return "", errors.New("unexpected value")
				}
				return bar, nil
			}).
			Apply(func (v string) ([]string, error) {
				strs := strings.Split(v, ",")
				if len(strs) != 2 {
					return nil, errors.New("unexpected value")
				}
				return []string{strs[0], strs[1]}, nil
			})

		res2 := out.
			Apply(func(v int) myStructType {
				return myStructType{foo: v, bar: "foo,bar"}
			}).
			Apply(func(v interface{}) (string, error) {
				bar := v.(myStructType).bar
				if bar != "foo,bar" {
					return "", errors.New("unexpected value")
				}
				return bar, nil
			}).
			Apply(func (v string) ([]string, error) {
				strs := strings.Split(v, ",")
				if len(strs) != 2 {
					return nil, errors.New("unexpected value")
				}
				return []string{strs[0], strs[1]}, nil
			})

		res3 := All(res, res2).Apply(func (v []interface{}) string {
			res, res2 := v[0].([]string), v[1].([]string)
			return strings.Join(append(res2, res...), ",")
		})

		res4 := All(out, out2).Apply(func(v []interface{}) *myStructType {
			return &myStructType{
				foo: v[0].(int),
				bar: v[1].(string),
			}
		})

		_, ok := res.(StringArrayOutput)
		assert.True(t, ok)

		v, known, err := await(res)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, []string{"qux", "zed"}, v)

		_, ok = res2.(StringArrayOutput)
		assert.True(t, ok)

		v, known, err = await(res2)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, []string{"foo", "bar"}, v)

		_, ok = res3.(StringOutput)
		assert.True(t, ok)

		v, known, err = await(res3)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, "foo,bar,qux,zed", v)

		_, ok = res4.(AnyOutput)
		assert.True(t, ok)

		v, known, err = await(res4)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.Equal(t, &myStructType{foo: 42, bar: "hello"}, v)
	}
}
