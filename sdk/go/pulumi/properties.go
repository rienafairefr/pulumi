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
	"sync"

	"github.com/pkg/errors"
)

const (
	outputPending = iota
	outputResolved
	outputRejected
)

// Output helps encode the relationship between resources in a Pulumi application. Specifically an output property
// holds onto a value and the resource it came from. An output value can then be provided when constructing new
// resources, allowing that new resource to know both the value as well as the resource the value came from.  This
// allows for a precise "dependency graph" to be created, which properly tracks the relationship between resources.
type Output struct {
	*outputState // protect against value aliasing.
}

// outputState is a heap-allocated block of state for each output property, in case of aliasing.
type outputState struct {
	mutex sync.Mutex
	cond  *sync.Cond

	state uint32 // one of output{Pending,Resolved,Rejected}

	value interface{} // the value of this output if it is resolved.
	err   error       // the error associated with this output if it is rejected.
	known bool        // true if this output's value is known.

	deps []Resource // the dependencies associated with this output property.
}

func (o *outputState) dependencies() []Resource {
	if o == nil {
		return nil
	}
	return o.deps
}

func (o *outputState) fulfill(value interface{}, known bool, err error) {
	if o == nil {
		return
	}

	o.mutex.Lock()
	defer func() {
		o.mutex.Unlock()
		o.cond.Broadcast()
	}()

	if o.state != outputPending {
		return
	}

	if err != nil {
		o.state, o.err, o.known = outputRejected, err, true
	} else {
		o.state, o.value, o.known = outputResolved, value, known
	}
}

func (o *outputState) resolve(value interface{}, known bool) {
	o.fulfill(value, known, nil)
}

func (o *outputState) reject(err error) {
	o.fulfill(nil, true, err)
}

func (o *outputState) await(ctx context.Context) (interface{}, bool, error) {
	for {
		if o == nil {
			// If the state is nil, treat its value as resolved and unknown.
			return nil, false, nil
		}

		o.mutex.Lock()
		for o.state == outputPending {
			if ctx.Err() != nil {
				return nil, true, ctx.Err()
			}
			o.cond.Wait()
		}
		o.mutex.Unlock()

		if !o.known || o.err != nil {
			return nil, o.known, o.err
		}

		ov, ok := isOutput(o.value)
		if !ok {
			return o.value, true, nil
		}
		o = ov.outputState
	}
}

func newOutput(deps ...Resource) Output {
	out := Output{
		&outputState{
			deps: deps,
		},
	}
	out.outputState.cond = sync.NewCond(&out.outputState.mutex)
	return out
}

var outputType = reflect.TypeOf(Output{})

func isOutput(v interface{}) (Output, bool) {
	if v != nil {
		rv := reflect.ValueOf(v)
		if rv.Type().ConvertibleTo(outputType) {
			return rv.Convert(outputType).Interface().(Output), true
		}
	}
	return Output{}, false
}

// NewOutput returns an output value that can be used to rendezvous with the production of a value or error.  The
// function returns the output itself, plus two functions: one for resolving a value, and another for rejecting with an
// error; exactly one function must be called. This acts like a promise.
func NewOutput() (Output, func(interface{}), func(error)) {
	out := newOutput()

	resolve := func(v interface{}) {
		out.resolve(v, true)
	}
	reject := func(err error) {
		out.reject(err)
	}

	return out, resolve, reject
}

// ApplyWithContext transforms the data of the output property using the applier func. The result remains an output
// property, and accumulates all implicated dependencies, so that resources can be properly tracked using a DAG.
// This function does not block awaiting the value; instead, it spawns a Goroutine that will await its availability.
func (out Output) Apply(applier func(v interface{}) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext transforms the data of the output property using the applier func. The result remains an output
// property, and accumulates all implicated dependencies, so that resources can be properly tracked using a DAG.
// This function does not block awaiting the value; instead, it spawns a Goroutine that will await its availability.
// The provided context can be used to reject the output as canceled.
func (out Output) ApplyWithContext(ctx context.Context,
	applier func(ctx context.Context, v interface{}) (interface{}, error)) Output {

	result := newOutput(out.dependencies()...)
	go func() {
		v, known, err := out.await(ctx)
		if err != nil || !known {
			result.fulfill(nil, known, err)
			return
		}

		// If we have a known value, run the applier to transform it.
		u, err := applier(ctx, v)
		if err != nil {
			result.reject(err)
			return
		}

		// Fulfill the result.
		result.fulfill(u, true, nil)
	}()
	return result
}

// Input is the type of a generic input value for a Pulumi resource. This type is used in conjunction with Output
// to provide polymorphism over strongly-typed input values.
//
// The intended pattern for nested Pulumi value types is to define an input interface and a plain, input, and output
// variant of the value type that implement the input interface.
//
// For example, given a nested Pulumi value type with the following shape:
//
//     type Nested struct {
//         Foo int
//         Bar string
//     }
//
// We would define the following:
//
//     var nestedType = reflect.TypeOf((*Nested)(nil))
//
//     type NestedInputType interface {
//         pulumi.Input
//
//         isNested()
//     }
//
//     type Nested struct {
//         Foo int `pulumi:"foo"`
//         Bar string `pulumi:"bar"`
//     }
//
//     func (*Nested) ElementType() reflect.Type {
//         return nestedType
//     }
//
//     func (*Nested) isNested() {}
//
//     type NestedInput struct {
//         Foo pulumi.IntInput `pulumi:"foo"`
//         Bar pulumi.StringInput `pulumi:"bar"`
//     }
//
//     func (*NestedInput) ElementType() reflect.Type {
//         return nestedType
//     }
//
//     func (*NestedInput) isNested() {}
//
//     type NestedOutput pulumi.Output
//
//     func (NestedOutput) ElementType() reflect.Type {
//         return nestedType
//     }
//
//     func (NestedOutput) isNested() {}
//
//     func (out NestedOutput) Apply(applier func(*Nested) (interface{}, error)) {
//         return out.ApplyWithContext(context.Background(), func(_ context.Context, v *Nested) (interface{}, error) {
//             return applier(v)
//         })
//     }
//
//     func (out NestedOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, *Nested) (interface{}, error) {
//         return pulumi.Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
//             return applier(ctx, v.(*Nested))
//         })
//     }
//
type Input interface {
	ElementType() reflect.Type
}

var anyType = reflect.TypeOf((*interface{})(nil)).Elem()

type AnyInput interface {
	Input

	// nolint: unused
	isAny()
}

type anyInput struct {
	v interface{}
}

func Any(v interface{}) AnyInput {
	return anyInput{v: v}
}

func (anyInput) ElementType() reflect.Type {
	return anyType
}

func (anyInput) isAny() {}

type AnyOutput Output

func (AnyOutput) ElementType() reflect.Type {
	return anyType
}

func (AnyOutput) isAny() {}

// Apply applies a transformation to the any value when it is available.
func (out AnyOutput) Apply(applier func(interface{}) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out AnyOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, interface{}) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, applier)
}

var archiveType = reflect.TypeOf((*archive)(nil))

type ArchiveInput interface {
	Input

	// nolint: unused
	isArchive()
}

func (*archive) ElementType() reflect.Type {
	return archiveType
}

func (*archive) isArchive() {}

// ArchiveOutput is an Output that is typed to return archive values.
type ArchiveOutput Output

func (ArchiveOutput) ElementType() reflect.Type {
	return archiveType
}

func (ArchiveOutput) isArchive() {}

// Apply applies a transformation to the archive value when it is available.
func (out ArchiveOutput) Apply(applier func(Archive) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v Archive) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out ArchiveOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, Archive) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(Archive))
	})
}

var arrayType = reflect.TypeOf((*[]interface{})(nil)).Elem()

type ArrayInput interface {
	Input

	// nolint: unused
	isArray()
}

type Array []interface{}

func (Array) ElementType() reflect.Type {
	return arrayType
}

func (Array) isArray() {}

// ArrayOutput is an Output that is typed to return arrays of values.
type ArrayOutput Output

func (ArrayOutput) ElementType() reflect.Type {
	return arrayType
}

func (ArrayOutput) isArray() {}

// Apply applies a transformation to the archive value when it is available.
func (out ArrayOutput) Apply(applier func([]interface{}) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v []interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out ArrayOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, []interface{}) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	})
}

var assetType = reflect.TypeOf((*asset)(nil))

type AssetInput interface {
	Input

	// nolint: unused
	isAsset()
}

func (*asset) ElementType() reflect.Type {
	return assetType
}

func (*asset) isAsset() {}

// AssetOutput is an Output that is typed to return asset values.
type AssetOutput Output

func (AssetOutput) ElementType() reflect.Type {
	return assetType
}

func (AssetOutput) isAsset() {}

// Apply applies a transformation to the archive value when it is available.
func (out AssetOutput) Apply(applier func(Asset) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v Asset) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out AssetOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, Asset) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(Asset))
	})
}

var boolType = reflect.TypeOf(false)

type BoolInput interface {
	Input

	// nolint: unused
	isBool()
}

type Bool bool

func (Bool) ElementType() reflect.Type {
	return boolType
}

func (Bool) isBool() {}

// BoolOutput is an Output that is typed to return bool values.
type BoolOutput Output

func (BoolOutput) ElementType() reflect.Type {
	return boolType
}

func (BoolOutput) isBool() {}

// Apply applies a transformation to the archive value when it is available.
func (out BoolOutput) Apply(applier func(bool) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v bool) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out BoolOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, bool) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	})
}

var float32Type = reflect.TypeOf(float32(0))

type Float32Input interface {
	Input

	// nolint: unused
	isFloat32()
}

type Float32 float32

func (Float32) ElementType() reflect.Type {
	return float32Type
}

func (Float32) isFloat32() {}

// Float32Output is an Output that is typed to return float32 values.
type Float32Output Output

func (Float32Output) ElementType() reflect.Type {
	return float32Type
}

func (Float32Output) isFloat32() {}

// Apply applies a transformation to the archive value when it is available.
func (out Float32Output) Apply(applier func(float32) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v float32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Float32Output) ApplyWithContext(ctx context.Context, applier func(context.Context, float32) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	})
}

var float64Type = reflect.TypeOf(float64(0))

type Float64Input interface {
	Input

	// nolint: unused
	isFloat64()
}

type Float64 float64

func (Float64) ElementType() reflect.Type {
	return float64Type
}

func (Float64) isFloat64() {}

// Float64Output is an Output that is typed to return float64 values.
type Float64Output Output

func (Float64Output) ElementType() reflect.Type {
	return float64Type
}

func (Float64Output) isFloat64() {}

// Apply applies a transformation to the archive value when it is available.
func (out Float64Output) Apply(applier func(float64) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v float64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Float64Output) ApplyWithContext(ctx context.Context, applier func(context.Context, float64) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	})
}

var stringType = reflect.TypeOf("")

var idType = reflect.TypeOf(ID(""))

type IDInput interface {
	Input

	// nolint: unused
	isID()
}

func (ID) ElementType() reflect.Type {
	return idType
}

func (ID) isID() {}

// IDOutput is an Output that is typed to return ID values.
type IDOutput Output

func (IDOutput) ElementType() reflect.Type {
	return idType
}

func (IDOutput) isID() {}

func (out IDOutput) await(ctx context.Context) (ID, bool, error) {
	id, known, err := Output(out).await(ctx)
	if !known || err != nil {
		return "", known, err
	}
	return ID(convert(id, stringType).(string)), true, nil
}

// Apply applies a transformation to the archive value when it is available.
func (out IDOutput) Apply(applier func(ID) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v ID) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out IDOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, ID) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, ID(convert(v, stringType).(string)))
	})
}

var intType = reflect.TypeOf(int(0))

type IntInput interface {
	Input

	// nolint: unused
	isInt()
}

type Int int

func (Int) ElementType() reflect.Type {
	return intType
}

func (Int) isInt() {}

// IntOutput is an Output that is typed to return int values.
type IntOutput Output

func (IntOutput) ElementType() reflect.Type {
	return intType
}

func (IntOutput) isInt() {}

// Apply applies a transformation to the archive value when it is available.
func (out IntOutput) Apply(applier func(int) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out IntOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, int) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	})
}

var int8Type = reflect.TypeOf(int8(0))

type Int8Input interface {
	Input

	// nolint: unused
	isInt8()
}

type Int8 int

func (Int8) ElementType() reflect.Type {
	return int8Type
}

func (Int8) isInt8() {}

// Int8Output is an Output that is typed to return int8 values.
type Int8Output Output

func (Int8Output) ElementType() reflect.Type {
	return int8Type
}

func (Int8Output) isInt8() {}

// Apply applies a transformation to the archive value when it is available.
func (out Int8Output) Apply(applier func(int8) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int8) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Int8Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int8) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	})
}

var int16Type = reflect.TypeOf(int16(0))

type Int16Input interface {
	Input

	// nolint: unused
	isInt16()
}

type Int16 int

func (Int16) ElementType() reflect.Type {
	return int16Type
}

func (Int16) isInt16() {}

// Int16Output is an Output that is typed to return int16 values.
type Int16Output Output

func (Int16Output) ElementType() reflect.Type {
	return int16Type
}

func (Int16Output) isInt16() {}

// Apply applies a transformation to the archive value when it is available.
func (out Int16Output) Apply(applier func(int16) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int16) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Int16Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int16) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	})
}

var int32Type = reflect.TypeOf(int32(0))

type Int32Input interface {
	Input

	// nolint: unused
	isInt32()
}

type Int32 int

func (Int32) ElementType() reflect.Type {
	return int32Type
}

func (Int32) isInt32() {}

// Int32Output is an Output that is typed to return int32 values.
type Int32Output Output

func (Int32Output) ElementType() reflect.Type {
	return int32Type
}

func (Int32Output) isInt32() {}

// Apply applies a transformation to the archive value when it is available.
func (out Int32Output) Apply(applier func(int32) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Int32Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int32) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	})
}

var int64Type = reflect.TypeOf(int64(0))

type Int64Input interface {
	Input

	// nolint: unused
	isInt64()
}

type Int64 int

func (Int64) ElementType() reflect.Type {
	return int64Type
}

func (Int64) isInt64() {}

// Int64Output is an Output that is typed to return int64 values.
type Int64Output Output

func (Int64Output) ElementType() reflect.Type {
	return int64Type
}

func (Int64Output) isInt64() {}

// Apply applies a transformation to the archive value when it is available.
func (out Int64Output) Apply(applier func(int64) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Int64Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int64) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	})
}

var mapType = reflect.TypeOf((*map[string]interface{})(nil)).Elem()

type MapInput interface {
	Input

	// nolint: unused
	isMap()
}

type Map map[string]interface{}

func (Map) ElementType() reflect.Type {
	return mapType
}

func (Map) isMap() {}

// MapOutput is an Output that is typed to return map values.
type MapOutput Output

func (MapOutput) ElementType() reflect.Type {
	return mapType
}

func (MapOutput) isMap() {}

// Apply applies a transformation to the number value when it is available.
func (out MapOutput) Apply(applier func(map[string]interface{}) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the number value when it is available.
func (out MapOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	})
}

type StringInput interface {
	Input

	// nolint: unused
	isString()
}

type String string

func (String) ElementType() reflect.Type {
	return stringType
}

func (String) isString() {}

// StringOutput is an Output that is typed to return number values.
type StringOutput Output

func (StringOutput) ElementType() reflect.Type {
	return stringType
}

func (StringOutput) isString() {}

// Apply applies a transformation to the archive value when it is available.
func (out StringOutput) Apply(applier func(string) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v string) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out StringOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, string) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	})
}

var uintType = reflect.TypeOf(uint(0))

type UintInput interface {
	Input

	// nolint: unused
	isUint()
}

type Uint uint

func (Uint) ElementType() reflect.Type {
	return uintType
}

func (Uint) isUint() {}

// UintOutput is an Output that is typed to return uint values.
type UintOutput Output

func (UintOutput) ElementType() reflect.Type {
	return uintType
}

func (UintOutput) isUint() {}

// Apply applies a transformation to the archive value when it is available.
func (out UintOutput) Apply(applier func(uint) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out UintOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, uint) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	})
}

var uint8Type = reflect.TypeOf(uint8(0))

type Uint8Input interface {
	Input

	// nolint: unused
	isUint8()
}

type Uint8 uint

func (Uint8) ElementType() reflect.Type {
	return uint8Type
}

func (Uint8) isUint8() {}

// Uint8Output is an Output that is typed to return uint8 values.
type Uint8Output Output

func (Uint8Output) ElementType() reflect.Type {
	return uint8Type
}

func (Uint8Output) isUint8() {}

// Apply applies a transformation to the archive value when it is available.
func (out Uint8Output) Apply(applier func(uint8) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint8) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Uint8Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint8) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	})
}

var uint16Type = reflect.TypeOf(uint16(0))

type Uint16Input interface {
	Input

	// nolint: unused
	isUint16()
}

type Uint16 uint

func (Uint16) ElementType() reflect.Type {
	return uint16Type
}

func (Uint16) isUint16() {}

// Uint16Output is an Output that is typed to return uint16 values.
type Uint16Output Output

func (Uint16Output) ElementType() reflect.Type {
	return uint16Type
}

func (Uint16Output) isUint16() {}

// Apply applies a transformation to the archive value when it is available.
func (out Uint16Output) Apply(applier func(uint16) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint16) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Uint16Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint16) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	})
}

var uint32Type = reflect.TypeOf(uint32(0))

type Uint32Input interface {
	Input

	// nolint: unused
	isUint32()
}

type Uint32 uint

func (Uint32) ElementType() reflect.Type {
	return uint32Type
}

func (Uint32) isUint32() {}

// Uint32Output is an Output that is typed to return uint32 values.
type Uint32Output Output

func (Uint32Output) ElementType() reflect.Type {
	return uint32Type
}

func (Uint32Output) isUint32() {}

// Apply applies a transformation to the archive value when it is available.
func (out Uint32Output) Apply(applier func(uint32) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Uint32Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint32) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	})
}

var uint64Type = reflect.TypeOf(uint64(0))

type Uint64Input interface {
	Input

	// nolint: unused
	isUint64()
}

type Uint64 uint

func (Uint64) ElementType() reflect.Type {
	return uint64Type
}

func (Uint64) isUint64() {}

// Uint64Output is an Output that is typed to return uint64 values.
type Uint64Output Output

func (Uint64Output) ElementType() reflect.Type {
	return uint64Type
}

func (Uint64Output) isUint64() {}

// Apply applies a transformation to the archive value when it is available.
func (out Uint64Output) Apply(applier func(uint64) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out Uint64Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint64) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	})
}

var urnType = reflect.TypeOf(URN(""))

type URNInput interface {
	Input

	// nolint: unused
	isURN()
}

func (URN) ElementType() reflect.Type {
	return urnType
}

func (URN) isURN() {}

// URNOutput is an Output that is typed to return URN values.
type URNOutput Output

func (URNOutput) ElementType() reflect.Type {
	return urnType
}

func (URNOutput) isURN() {}

func (out URNOutput) await(ctx context.Context) (URN, bool, error) {
	urn, known, err := Output(out).await(ctx)
	if !known || err != nil {
		return "", known, err
	}
	return URN(convert(urn, stringType).(string)), true, nil
}

// Apply applies a transformation to the archive value when it is available.
func (out URNOutput) Apply(applier func(URN) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v URN) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out URNOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, URN) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, URN(convert(v, stringType).(string)))
	})
}

func convert(v interface{}, to reflect.Type) interface{} {
	rv := reflect.ValueOf(v)
	if !rv.Type().ConvertibleTo(to) {
		panic(errors.Errorf("cannot convert output value of type %s to %s", rv.Type(), to))
	}
	return rv.Convert(to).Interface()
}
