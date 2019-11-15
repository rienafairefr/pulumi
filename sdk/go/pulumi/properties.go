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

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Output) ApplyAny(applier func(interface{}) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, interface{}) (interface{}, error)) AnyOutput {
	return AnyOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Output) ApplyArchive(applier func(interface{}) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v interface{}) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, interface{}) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Output) ApplyArray(applier func(interface{}) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v interface{}) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, interface{}) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Output) ApplyAsset(applier func(interface{}) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v interface{}) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, interface{}) (Asset, error)) AssetOutput {
	return AssetOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Output) ApplyAssetOrArchive(applier func(interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v interface{}) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Output) ApplyBool(applier func(interface{}) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v interface{}) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, interface{}) (bool, error)) BoolOutput {
	return BoolOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Output) ApplyFloat32(applier func(interface{}) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v interface{}) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, interface{}) (float32, error)) Float32Output {
	return Float32Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Output) ApplyFloat64(applier func(interface{}) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v interface{}) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, interface{}) (float64, error)) Float64Output {
	return Float64Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Output) ApplyID(applier func(interface{}) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v interface{}) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, interface{}) (ID, error)) IDOutput {
	return IDOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Output) ApplyInt(applier func(interface{}) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v interface{}) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, interface{}) (int, error)) IntOutput {
	return IntOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Output) ApplyInt16(applier func(interface{}) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v interface{}) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, interface{}) (int16, error)) Int16Output {
	return Int16Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Output) ApplyInt32(applier func(interface{}) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v interface{}) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, interface{}) (int32, error)) Int32Output {
	return Int32Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Output) ApplyInt64(applier func(interface{}) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v interface{}) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, interface{}) (int64, error)) Int64Output {
	return Int64Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Output) ApplyInt8(applier func(interface{}) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v interface{}) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, interface{}) (int8, error)) Int8Output {
	return Int8Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Output) ApplyMap(applier func(interface{}) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v interface{}) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, interface{}) (map[string]interface{}, error)) MapOutput {
	return MapOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Output) ApplyString(applier func(interface{}) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v interface{}) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, interface{}) (string, error)) StringOutput {
	return StringOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Output) ApplyURN(applier func(interface{}) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v interface{}) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, interface{}) (URN, error)) URNOutput {
	return URNOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Output) ApplyUint(applier func(interface{}) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v interface{}) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, interface{}) (uint, error)) UintOutput {
	return UintOutput(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Output) ApplyUint16(applier func(interface{}) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v interface{}) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint16, error)) Uint16Output {
	return Uint16Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Output) ApplyUint32(applier func(interface{}) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v interface{}) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint32, error)) Uint32Output {
	return Uint32Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Output) ApplyUint64(applier func(interface{}) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v interface{}) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint64, error)) Uint64Output {
	return Uint64Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Output) ApplyUint8(applier func(interface{}) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v interface{}) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint8, error)) Uint8Output {
	return Uint8Output(out.ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
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

type anyInput struct {
	v interface{}
}

func Any(v interface{}) AnyInput {
	return anyInput{v: v}
}

var anyType = reflect.TypeOf((*interface{})(nil)).Elem()

// AnyInput is an input type that accepts Any and AnyOutput values.
type AnyInput interface {
	Input

	// nolint: unused
	isAny()
}

// ElementType returns the element type of this Input (interface{}).
func (anyInput) ElementType() reflect.Type {
	return anyType
}

func (anyInput) isAny() {}

// AnyOutput is an Output that returns interface{} values.
type AnyOutput Output

// ElementType returns the element type of this Output (interface{}).
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

// ApplyWithContext applies a transformation to the any value when it is available.
func (out AnyOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, interface{}) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out AnyOutput) ApplyAny(applier func(v interface{}) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out AnyOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, interface{}) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out AnyOutput) ApplyArchive(applier func(v interface{}) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v interface{}) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out AnyOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, interface{}) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out AnyOutput) ApplyArray(applier func(v interface{}) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v interface{}) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out AnyOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, interface{}) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out AnyOutput) ApplyAsset(applier func(v interface{}) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v interface{}) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out AnyOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, interface{}) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out AnyOutput) ApplyAssetOrArchive(applier func(v interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v interface{}) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out AnyOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out AnyOutput) ApplyBool(applier func(v interface{}) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v interface{}) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out AnyOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, interface{}) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out AnyOutput) ApplyFloat32(applier func(v interface{}) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v interface{}) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out AnyOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, interface{}) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out AnyOutput) ApplyFloat64(applier func(v interface{}) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v interface{}) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out AnyOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, interface{}) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out AnyOutput) ApplyID(applier func(v interface{}) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v interface{}) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out AnyOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, interface{}) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out AnyOutput) ApplyInt(applier func(v interface{}) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v interface{}) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out AnyOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, interface{}) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out AnyOutput) ApplyInt16(applier func(v interface{}) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v interface{}) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out AnyOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, interface{}) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out AnyOutput) ApplyInt32(applier func(v interface{}) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v interface{}) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out AnyOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, interface{}) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out AnyOutput) ApplyInt64(applier func(v interface{}) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v interface{}) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out AnyOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, interface{}) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out AnyOutput) ApplyInt8(applier func(v interface{}) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v interface{}) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out AnyOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, interface{}) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out AnyOutput) ApplyMap(applier func(v interface{}) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v interface{}) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out AnyOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, interface{}) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out AnyOutput) ApplyString(applier func(v interface{}) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v interface{}) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out AnyOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, interface{}) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out AnyOutput) ApplyURN(applier func(v interface{}) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v interface{}) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out AnyOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, interface{}) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out AnyOutput) ApplyUint(applier func(v interface{}) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v interface{}) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out AnyOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, interface{}) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out AnyOutput) ApplyUint16(applier func(v interface{}) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v interface{}) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out AnyOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out AnyOutput) ApplyUint32(applier func(v interface{}) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v interface{}) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out AnyOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out AnyOutput) ApplyUint64(applier func(v interface{}) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v interface{}) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out AnyOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out AnyOutput) ApplyUint8(applier func(v interface{}) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v interface{}) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out AnyOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, interface{}) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, v)
	}))
}

var archiveType = reflect.TypeOf((**archive)(nil)).Elem()

// ArchiveInput is an input type that accepts Archive and ArchiveOutput values.
type ArchiveInput interface {
	Input

	// nolint: unused
	isArchive()
}

// ElementType returns the element type of this Input (*archive).
func (*archive) ElementType() reflect.Type {
	return archiveType
}

func (*archive) isArchive() {}

func (*archive) isAssetOrArchive() {}

// ArchiveOutput is an Output that returns Archive values.
type ArchiveOutput Output

// ElementType returns the element type of this Output (Archive).
func (ArchiveOutput) ElementType() reflect.Type {
	return archiveType
}

func (ArchiveOutput) isArchive() {}

func (ArchiveOutput) isAssetOrArchive() {}

// Apply applies a transformation to the archive value when it is available.
func (out ArchiveOutput) Apply(applier func(Archive) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v Archive) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the archive value when it is available.
func (out ArchiveOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, Archive) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out ArchiveOutput) ApplyAny(applier func(v Archive) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v Archive) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out ArchiveOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, Archive) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out ArchiveOutput) ApplyArchive(applier func(v Archive) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v Archive) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out ArchiveOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, Archive) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out ArchiveOutput) ApplyArray(applier func(v Archive) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v Archive) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out ArchiveOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, Archive) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out ArchiveOutput) ApplyAsset(applier func(v Archive) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v Archive) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out ArchiveOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, Archive) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out ArchiveOutput) ApplyAssetOrArchive(applier func(v Archive) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v Archive) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out ArchiveOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, Archive) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out ArchiveOutput) ApplyBool(applier func(v Archive) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v Archive) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out ArchiveOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, Archive) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out ArchiveOutput) ApplyFloat32(applier func(v Archive) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v Archive) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out ArchiveOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, Archive) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out ArchiveOutput) ApplyFloat64(applier func(v Archive) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v Archive) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out ArchiveOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, Archive) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out ArchiveOutput) ApplyID(applier func(v Archive) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v Archive) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out ArchiveOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, Archive) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out ArchiveOutput) ApplyInt(applier func(v Archive) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v Archive) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out ArchiveOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, Archive) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out ArchiveOutput) ApplyInt16(applier func(v Archive) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v Archive) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out ArchiveOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, Archive) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out ArchiveOutput) ApplyInt32(applier func(v Archive) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v Archive) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out ArchiveOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, Archive) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out ArchiveOutput) ApplyInt64(applier func(v Archive) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v Archive) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out ArchiveOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, Archive) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out ArchiveOutput) ApplyInt8(applier func(v Archive) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v Archive) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out ArchiveOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, Archive) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out ArchiveOutput) ApplyMap(applier func(v Archive) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v Archive) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out ArchiveOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, Archive) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out ArchiveOutput) ApplyString(applier func(v Archive) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v Archive) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out ArchiveOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, Archive) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out ArchiveOutput) ApplyURN(applier func(v Archive) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v Archive) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out ArchiveOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, Archive) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out ArchiveOutput) ApplyUint(applier func(v Archive) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v Archive) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out ArchiveOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, Archive) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out ArchiveOutput) ApplyUint16(applier func(v Archive) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v Archive) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out ArchiveOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, Archive) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out ArchiveOutput) ApplyUint32(applier func(v Archive) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v Archive) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out ArchiveOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, Archive) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out ArchiveOutput) ApplyUint64(applier func(v Archive) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v Archive) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out ArchiveOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, Archive) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out ArchiveOutput) ApplyUint8(applier func(v Archive) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v Archive) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out ArchiveOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, Archive) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, archiveType).(*archive))
	}))
}

var arrayType = reflect.TypeOf((*[]interface{})(nil)).Elem()

// ArrayInput is an input type that accepts Array and ArrayOutput values.
type ArrayInput interface {
	Input

	// nolint: unused
	isArray()
}

// Array is an input type for []interface{} values.
type Array []interface{}

// ElementType returns the element type of this Input ([]interface{}).
func (Array) ElementType() reflect.Type {
	return arrayType
}

func (Array) isArray() {}

// ArrayOutput is an Output that returns []interface{} values.
type ArrayOutput Output

// ElementType returns the element type of this Output ([]interface{}).
func (ArrayOutput) ElementType() reflect.Type {
	return arrayType
}

func (ArrayOutput) isArray() {}

// Apply applies a transformation to the array value when it is available.
func (out ArrayOutput) Apply(applier func([]interface{}) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v []interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the array value when it is available.
func (out ArrayOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, []interface{}) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out ArrayOutput) ApplyAny(applier func(v []interface{}) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v []interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out ArrayOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, []interface{}) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out ArrayOutput) ApplyArchive(applier func(v []interface{}) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v []interface{}) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out ArrayOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, []interface{}) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out ArrayOutput) ApplyArray(applier func(v []interface{}) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v []interface{}) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out ArrayOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, []interface{}) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out ArrayOutput) ApplyAsset(applier func(v []interface{}) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v []interface{}) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out ArrayOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, []interface{}) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out ArrayOutput) ApplyAssetOrArchive(applier func(v []interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v []interface{}) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out ArrayOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, []interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out ArrayOutput) ApplyBool(applier func(v []interface{}) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v []interface{}) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out ArrayOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, []interface{}) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out ArrayOutput) ApplyFloat32(applier func(v []interface{}) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v []interface{}) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out ArrayOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, []interface{}) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out ArrayOutput) ApplyFloat64(applier func(v []interface{}) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v []interface{}) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out ArrayOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, []interface{}) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out ArrayOutput) ApplyID(applier func(v []interface{}) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v []interface{}) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out ArrayOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, []interface{}) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out ArrayOutput) ApplyInt(applier func(v []interface{}) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v []interface{}) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out ArrayOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, []interface{}) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out ArrayOutput) ApplyInt16(applier func(v []interface{}) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v []interface{}) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out ArrayOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, []interface{}) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out ArrayOutput) ApplyInt32(applier func(v []interface{}) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v []interface{}) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out ArrayOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, []interface{}) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out ArrayOutput) ApplyInt64(applier func(v []interface{}) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v []interface{}) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out ArrayOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, []interface{}) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out ArrayOutput) ApplyInt8(applier func(v []interface{}) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v []interface{}) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out ArrayOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, []interface{}) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out ArrayOutput) ApplyMap(applier func(v []interface{}) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v []interface{}) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out ArrayOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, []interface{}) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out ArrayOutput) ApplyString(applier func(v []interface{}) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v []interface{}) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out ArrayOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, []interface{}) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out ArrayOutput) ApplyURN(applier func(v []interface{}) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v []interface{}) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out ArrayOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, []interface{}) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out ArrayOutput) ApplyUint(applier func(v []interface{}) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v []interface{}) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out ArrayOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, []interface{}) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out ArrayOutput) ApplyUint16(applier func(v []interface{}) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v []interface{}) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out ArrayOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, []interface{}) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out ArrayOutput) ApplyUint32(applier func(v []interface{}) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v []interface{}) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out ArrayOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, []interface{}) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out ArrayOutput) ApplyUint64(applier func(v []interface{}) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v []interface{}) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out ArrayOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, []interface{}) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out ArrayOutput) ApplyUint8(applier func(v []interface{}) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v []interface{}) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out ArrayOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, []interface{}) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, arrayType).([]interface{}))
	}))
}

var assetType = reflect.TypeOf((**asset)(nil)).Elem()

// AssetInput is an input type that accepts Asset and AssetOutput values.
type AssetInput interface {
	Input

	// nolint: unused
	isAsset()
}

// ElementType returns the element type of this Input (*asset).
func (*asset) ElementType() reflect.Type {
	return assetType
}

func (*asset) isAsset() {}

func (*asset) isAssetOrArchive() {}

// AssetOutput is an Output that returns Asset values.
type AssetOutput Output

// ElementType returns the element type of this Output (Asset).
func (AssetOutput) ElementType() reflect.Type {
	return assetType
}

func (AssetOutput) isAsset() {}

func (AssetOutput) isAssetOrArchive() {}

// Apply applies a transformation to the asset value when it is available.
func (out AssetOutput) Apply(applier func(Asset) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v Asset) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the asset value when it is available.
func (out AssetOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, Asset) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out AssetOutput) ApplyAny(applier func(v Asset) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v Asset) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out AssetOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, Asset) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out AssetOutput) ApplyArchive(applier func(v Asset) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v Asset) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out AssetOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, Asset) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out AssetOutput) ApplyArray(applier func(v Asset) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v Asset) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out AssetOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, Asset) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out AssetOutput) ApplyAsset(applier func(v Asset) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v Asset) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out AssetOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, Asset) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out AssetOutput) ApplyAssetOrArchive(applier func(v Asset) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v Asset) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out AssetOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, Asset) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out AssetOutput) ApplyBool(applier func(v Asset) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v Asset) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out AssetOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, Asset) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out AssetOutput) ApplyFloat32(applier func(v Asset) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v Asset) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out AssetOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, Asset) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out AssetOutput) ApplyFloat64(applier func(v Asset) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v Asset) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out AssetOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, Asset) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out AssetOutput) ApplyID(applier func(v Asset) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v Asset) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out AssetOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, Asset) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out AssetOutput) ApplyInt(applier func(v Asset) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v Asset) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out AssetOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, Asset) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out AssetOutput) ApplyInt16(applier func(v Asset) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v Asset) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out AssetOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, Asset) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out AssetOutput) ApplyInt32(applier func(v Asset) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v Asset) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out AssetOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, Asset) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out AssetOutput) ApplyInt64(applier func(v Asset) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v Asset) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out AssetOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, Asset) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out AssetOutput) ApplyInt8(applier func(v Asset) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v Asset) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out AssetOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, Asset) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out AssetOutput) ApplyMap(applier func(v Asset) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v Asset) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out AssetOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, Asset) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out AssetOutput) ApplyString(applier func(v Asset) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v Asset) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out AssetOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, Asset) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out AssetOutput) ApplyURN(applier func(v Asset) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v Asset) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out AssetOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, Asset) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out AssetOutput) ApplyUint(applier func(v Asset) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v Asset) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out AssetOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, Asset) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out AssetOutput) ApplyUint16(applier func(v Asset) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v Asset) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out AssetOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, Asset) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out AssetOutput) ApplyUint32(applier func(v Asset) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v Asset) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out AssetOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, Asset) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out AssetOutput) ApplyUint64(applier func(v Asset) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v Asset) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out AssetOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, Asset) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out AssetOutput) ApplyUint8(applier func(v Asset) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v Asset) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out AssetOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, Asset) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetType).(*asset))
	}))
}

var assetorarchiveType = reflect.TypeOf((*AssetOrArchive)(nil)).Elem()

// AssetOrArchiveInput is an input type that accepts AssetOrArchive and AssetOrArchiveOutput values.
type AssetOrArchiveInput interface {
	Input

	// nolint: unused
	isAssetOrArchive()
}

// AssetOrArchiveOutput is an Output that returns AssetOrArchive values.
type AssetOrArchiveOutput Output

// ElementType returns the element type of this Output (AssetOrArchive).
func (AssetOrArchiveOutput) ElementType() reflect.Type {
	return assetorarchiveType
}

func (AssetOrArchiveOutput) isAssetOrArchive() {}

// Apply applies a transformation to the assetorarchive value when it is available.
func (out AssetOrArchiveOutput) Apply(applier func(AssetOrArchive) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the assetorarchive value when it is available.
func (out AssetOrArchiveOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out AssetOrArchiveOutput) ApplyAny(applier func(v AssetOrArchive) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out AssetOrArchiveOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out AssetOrArchiveOutput) ApplyArchive(applier func(v AssetOrArchive) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out AssetOrArchiveOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out AssetOrArchiveOutput) ApplyArray(applier func(v AssetOrArchive) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out AssetOrArchiveOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out AssetOrArchiveOutput) ApplyAsset(applier func(v AssetOrArchive) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out AssetOrArchiveOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out AssetOrArchiveOutput) ApplyAssetOrArchive(applier func(v AssetOrArchive) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out AssetOrArchiveOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out AssetOrArchiveOutput) ApplyBool(applier func(v AssetOrArchive) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out AssetOrArchiveOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out AssetOrArchiveOutput) ApplyFloat32(applier func(v AssetOrArchive) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out AssetOrArchiveOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out AssetOrArchiveOutput) ApplyFloat64(applier func(v AssetOrArchive) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out AssetOrArchiveOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out AssetOrArchiveOutput) ApplyID(applier func(v AssetOrArchive) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out AssetOrArchiveOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out AssetOrArchiveOutput) ApplyInt(applier func(v AssetOrArchive) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out AssetOrArchiveOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out AssetOrArchiveOutput) ApplyInt16(applier func(v AssetOrArchive) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out AssetOrArchiveOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out AssetOrArchiveOutput) ApplyInt32(applier func(v AssetOrArchive) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out AssetOrArchiveOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out AssetOrArchiveOutput) ApplyInt64(applier func(v AssetOrArchive) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out AssetOrArchiveOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out AssetOrArchiveOutput) ApplyInt8(applier func(v AssetOrArchive) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out AssetOrArchiveOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out AssetOrArchiveOutput) ApplyMap(applier func(v AssetOrArchive) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out AssetOrArchiveOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out AssetOrArchiveOutput) ApplyString(applier func(v AssetOrArchive) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out AssetOrArchiveOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out AssetOrArchiveOutput) ApplyURN(applier func(v AssetOrArchive) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out AssetOrArchiveOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out AssetOrArchiveOutput) ApplyUint(applier func(v AssetOrArchive) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out AssetOrArchiveOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out AssetOrArchiveOutput) ApplyUint16(applier func(v AssetOrArchive) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out AssetOrArchiveOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out AssetOrArchiveOutput) ApplyUint32(applier func(v AssetOrArchive) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out AssetOrArchiveOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out AssetOrArchiveOutput) ApplyUint64(applier func(v AssetOrArchive) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out AssetOrArchiveOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out AssetOrArchiveOutput) ApplyUint8(applier func(v AssetOrArchive) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v AssetOrArchive) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out AssetOrArchiveOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, AssetOrArchive) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, assetorarchiveType).(AssetOrArchive))
	}))
}

var boolType = reflect.TypeOf((*bool)(nil)).Elem()

// BoolInput is an input type that accepts Bool and BoolOutput values.
type BoolInput interface {
	Input

	// nolint: unused
	isBool()
}

// Bool is an input type for bool values.
type Bool bool

// ElementType returns the element type of this Input (bool).
func (Bool) ElementType() reflect.Type {
	return boolType
}

func (Bool) isBool() {}

// BoolOutput is an Output that returns bool values.
type BoolOutput Output

// ElementType returns the element type of this Output (bool).
func (BoolOutput) ElementType() reflect.Type {
	return boolType
}

func (BoolOutput) isBool() {}

// Apply applies a transformation to the bool value when it is available.
func (out BoolOutput) Apply(applier func(bool) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v bool) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the bool value when it is available.
func (out BoolOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, bool) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out BoolOutput) ApplyAny(applier func(v bool) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v bool) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out BoolOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, bool) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out BoolOutput) ApplyArchive(applier func(v bool) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v bool) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out BoolOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, bool) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out BoolOutput) ApplyArray(applier func(v bool) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v bool) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out BoolOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, bool) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out BoolOutput) ApplyAsset(applier func(v bool) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v bool) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out BoolOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, bool) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out BoolOutput) ApplyAssetOrArchive(applier func(v bool) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v bool) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out BoolOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, bool) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out BoolOutput) ApplyBool(applier func(v bool) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v bool) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out BoolOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, bool) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out BoolOutput) ApplyFloat32(applier func(v bool) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v bool) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out BoolOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, bool) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out BoolOutput) ApplyFloat64(applier func(v bool) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v bool) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out BoolOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, bool) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out BoolOutput) ApplyID(applier func(v bool) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v bool) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out BoolOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, bool) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out BoolOutput) ApplyInt(applier func(v bool) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v bool) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out BoolOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, bool) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out BoolOutput) ApplyInt16(applier func(v bool) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v bool) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out BoolOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, bool) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out BoolOutput) ApplyInt32(applier func(v bool) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v bool) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out BoolOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, bool) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out BoolOutput) ApplyInt64(applier func(v bool) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v bool) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out BoolOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, bool) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out BoolOutput) ApplyInt8(applier func(v bool) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v bool) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out BoolOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, bool) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out BoolOutput) ApplyMap(applier func(v bool) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v bool) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out BoolOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, bool) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out BoolOutput) ApplyString(applier func(v bool) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v bool) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out BoolOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, bool) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out BoolOutput) ApplyURN(applier func(v bool) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v bool) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out BoolOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, bool) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out BoolOutput) ApplyUint(applier func(v bool) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v bool) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out BoolOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, bool) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out BoolOutput) ApplyUint16(applier func(v bool) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v bool) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out BoolOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, bool) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out BoolOutput) ApplyUint32(applier func(v bool) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v bool) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out BoolOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, bool) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out BoolOutput) ApplyUint64(applier func(v bool) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v bool) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out BoolOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, bool) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out BoolOutput) ApplyUint8(applier func(v bool) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v bool) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out BoolOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, bool) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, boolType).(bool))
	}))
}

var float32Type = reflect.TypeOf((*float32)(nil)).Elem()

// Float32Input is an input type that accepts Float32 and Float32Output values.
type Float32Input interface {
	Input

	// nolint: unused
	isFloat32()
}

// Float32 is an input type for float32 values.
type Float32 float32

// ElementType returns the element type of this Input (float32).
func (Float32) ElementType() reflect.Type {
	return float32Type
}

func (Float32) isFloat32() {}

// Float32Output is an Output that returns float32 values.
type Float32Output Output

// ElementType returns the element type of this Output (float32).
func (Float32Output) ElementType() reflect.Type {
	return float32Type
}

func (Float32Output) isFloat32() {}

// Apply applies a transformation to the float32 value when it is available.
func (out Float32Output) Apply(applier func(float32) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v float32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the float32 value when it is available.
func (out Float32Output) ApplyWithContext(ctx context.Context, applier func(context.Context, float32) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Float32Output) ApplyAny(applier func(v float32) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v float32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Float32Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, float32) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Float32Output) ApplyArchive(applier func(v float32) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v float32) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Float32Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, float32) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Float32Output) ApplyArray(applier func(v float32) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v float32) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Float32Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, float32) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Float32Output) ApplyAsset(applier func(v float32) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v float32) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Float32Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, float32) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Float32Output) ApplyAssetOrArchive(applier func(v float32) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v float32) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Float32Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, float32) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Float32Output) ApplyBool(applier func(v float32) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v float32) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Float32Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, float32) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Float32Output) ApplyFloat32(applier func(v float32) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v float32) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Float32Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, float32) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Float32Output) ApplyFloat64(applier func(v float32) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v float32) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Float32Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, float32) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Float32Output) ApplyID(applier func(v float32) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v float32) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Float32Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, float32) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Float32Output) ApplyInt(applier func(v float32) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v float32) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Float32Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, float32) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Float32Output) ApplyInt16(applier func(v float32) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v float32) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Float32Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, float32) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Float32Output) ApplyInt32(applier func(v float32) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v float32) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Float32Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, float32) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Float32Output) ApplyInt64(applier func(v float32) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v float32) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Float32Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, float32) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Float32Output) ApplyInt8(applier func(v float32) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v float32) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Float32Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, float32) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Float32Output) ApplyMap(applier func(v float32) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v float32) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Float32Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, float32) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Float32Output) ApplyString(applier func(v float32) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v float32) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Float32Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, float32) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Float32Output) ApplyURN(applier func(v float32) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v float32) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Float32Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, float32) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Float32Output) ApplyUint(applier func(v float32) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v float32) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Float32Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, float32) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Float32Output) ApplyUint16(applier func(v float32) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v float32) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Float32Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, float32) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Float32Output) ApplyUint32(applier func(v float32) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v float32) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Float32Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, float32) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Float32Output) ApplyUint64(applier func(v float32) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v float32) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Float32Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, float32) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Float32Output) ApplyUint8(applier func(v float32) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v float32) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Float32Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, float32) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float32Type).(float32))
	}))
}

var float64Type = reflect.TypeOf((*float64)(nil)).Elem()

// Float64Input is an input type that accepts Float64 and Float64Output values.
type Float64Input interface {
	Input

	// nolint: unused
	isFloat64()
}

// Float64 is an input type for float64 values.
type Float64 float64

// ElementType returns the element type of this Input (float64).
func (Float64) ElementType() reflect.Type {
	return float64Type
}

func (Float64) isFloat64() {}

// Float64Output is an Output that returns float64 values.
type Float64Output Output

// ElementType returns the element type of this Output (float64).
func (Float64Output) ElementType() reflect.Type {
	return float64Type
}

func (Float64Output) isFloat64() {}

// Apply applies a transformation to the float64 value when it is available.
func (out Float64Output) Apply(applier func(float64) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v float64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the float64 value when it is available.
func (out Float64Output) ApplyWithContext(ctx context.Context, applier func(context.Context, float64) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Float64Output) ApplyAny(applier func(v float64) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v float64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Float64Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, float64) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Float64Output) ApplyArchive(applier func(v float64) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v float64) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Float64Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, float64) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Float64Output) ApplyArray(applier func(v float64) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v float64) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Float64Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, float64) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Float64Output) ApplyAsset(applier func(v float64) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v float64) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Float64Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, float64) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Float64Output) ApplyAssetOrArchive(applier func(v float64) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v float64) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Float64Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, float64) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Float64Output) ApplyBool(applier func(v float64) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v float64) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Float64Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, float64) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Float64Output) ApplyFloat32(applier func(v float64) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v float64) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Float64Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, float64) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Float64Output) ApplyFloat64(applier func(v float64) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v float64) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Float64Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, float64) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Float64Output) ApplyID(applier func(v float64) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v float64) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Float64Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, float64) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Float64Output) ApplyInt(applier func(v float64) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v float64) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Float64Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, float64) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Float64Output) ApplyInt16(applier func(v float64) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v float64) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Float64Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, float64) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Float64Output) ApplyInt32(applier func(v float64) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v float64) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Float64Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, float64) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Float64Output) ApplyInt64(applier func(v float64) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v float64) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Float64Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, float64) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Float64Output) ApplyInt8(applier func(v float64) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v float64) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Float64Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, float64) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Float64Output) ApplyMap(applier func(v float64) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v float64) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Float64Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, float64) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Float64Output) ApplyString(applier func(v float64) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v float64) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Float64Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, float64) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Float64Output) ApplyURN(applier func(v float64) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v float64) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Float64Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, float64) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Float64Output) ApplyUint(applier func(v float64) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v float64) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Float64Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, float64) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Float64Output) ApplyUint16(applier func(v float64) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v float64) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Float64Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, float64) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Float64Output) ApplyUint32(applier func(v float64) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v float64) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Float64Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, float64) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Float64Output) ApplyUint64(applier func(v float64) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v float64) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Float64Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, float64) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Float64Output) ApplyUint8(applier func(v float64) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v float64) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Float64Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, float64) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, float64Type).(float64))
	}))
}

var idType = reflect.TypeOf((*ID)(nil)).Elem()

// IDInput is an input type that accepts ID and IDOutput values.
type IDInput interface {
	Input

	// nolint: unused
	isID()
}

// ElementType returns the element type of this Input (ID).
func (ID) ElementType() reflect.Type {
	return idType
}

func (ID) isID() {}

func (ID) isString() {}

// IDOutput is an Output that returns ID values.
type IDOutput Output

// ElementType returns the element type of this Output (ID).
func (IDOutput) ElementType() reflect.Type {
	return idType
}

func (IDOutput) isID() {}

func (IDOutput) isString() {}

// Apply applies a transformation to the id value when it is available.
func (out IDOutput) Apply(applier func(ID) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v ID) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the id value when it is available.
func (out IDOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, ID) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out IDOutput) ApplyAny(applier func(v ID) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v ID) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out IDOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, ID) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out IDOutput) ApplyArchive(applier func(v ID) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v ID) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out IDOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, ID) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out IDOutput) ApplyArray(applier func(v ID) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v ID) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out IDOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, ID) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out IDOutput) ApplyAsset(applier func(v ID) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v ID) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out IDOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, ID) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out IDOutput) ApplyAssetOrArchive(applier func(v ID) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v ID) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out IDOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, ID) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out IDOutput) ApplyBool(applier func(v ID) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v ID) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out IDOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, ID) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out IDOutput) ApplyFloat32(applier func(v ID) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v ID) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out IDOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, ID) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out IDOutput) ApplyFloat64(applier func(v ID) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v ID) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out IDOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, ID) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out IDOutput) ApplyID(applier func(v ID) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v ID) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out IDOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, ID) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out IDOutput) ApplyInt(applier func(v ID) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v ID) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out IDOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, ID) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out IDOutput) ApplyInt16(applier func(v ID) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v ID) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out IDOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, ID) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out IDOutput) ApplyInt32(applier func(v ID) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v ID) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out IDOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, ID) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out IDOutput) ApplyInt64(applier func(v ID) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v ID) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out IDOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, ID) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out IDOutput) ApplyInt8(applier func(v ID) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v ID) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out IDOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, ID) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out IDOutput) ApplyMap(applier func(v ID) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v ID) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out IDOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, ID) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out IDOutput) ApplyString(applier func(v ID) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v ID) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out IDOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, ID) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out IDOutput) ApplyURN(applier func(v ID) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v ID) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out IDOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, ID) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out IDOutput) ApplyUint(applier func(v ID) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v ID) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out IDOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, ID) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out IDOutput) ApplyUint16(applier func(v ID) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v ID) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out IDOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, ID) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out IDOutput) ApplyUint32(applier func(v ID) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v ID) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out IDOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, ID) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out IDOutput) ApplyUint64(applier func(v ID) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v ID) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out IDOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, ID) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out IDOutput) ApplyUint8(applier func(v ID) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v ID) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out IDOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, ID) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, idType).(ID))
	}))
}

var intType = reflect.TypeOf((*int)(nil)).Elem()

// IntInput is an input type that accepts Int and IntOutput values.
type IntInput interface {
	Input

	// nolint: unused
	isInt()
}

// Int is an input type for int values.
type Int int

// ElementType returns the element type of this Input (int).
func (Int) ElementType() reflect.Type {
	return intType
}

func (Int) isInt() {}

// IntOutput is an Output that returns int values.
type IntOutput Output

// ElementType returns the element type of this Output (int).
func (IntOutput) ElementType() reflect.Type {
	return intType
}

func (IntOutput) isInt() {}

// Apply applies a transformation to the int value when it is available.
func (out IntOutput) Apply(applier func(int) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the int value when it is available.
func (out IntOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, int) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out IntOutput) ApplyAny(applier func(v int) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v int) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out IntOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, int) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out IntOutput) ApplyArchive(applier func(v int) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v int) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out IntOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, int) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out IntOutput) ApplyArray(applier func(v int) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v int) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out IntOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, int) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out IntOutput) ApplyAsset(applier func(v int) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v int) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out IntOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, int) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out IntOutput) ApplyAssetOrArchive(applier func(v int) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v int) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out IntOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, int) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out IntOutput) ApplyBool(applier func(v int) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v int) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out IntOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, int) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out IntOutput) ApplyFloat32(applier func(v int) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v int) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out IntOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, int) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out IntOutput) ApplyFloat64(applier func(v int) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v int) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out IntOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, int) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out IntOutput) ApplyID(applier func(v int) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v int) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out IntOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, int) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out IntOutput) ApplyInt(applier func(v int) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v int) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out IntOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, int) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out IntOutput) ApplyInt16(applier func(v int) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v int) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out IntOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, int) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out IntOutput) ApplyInt32(applier func(v int) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v int) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out IntOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, int) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out IntOutput) ApplyInt64(applier func(v int) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v int) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out IntOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, int) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out IntOutput) ApplyInt8(applier func(v int) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v int) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out IntOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, int) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out IntOutput) ApplyMap(applier func(v int) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v int) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out IntOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, int) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out IntOutput) ApplyString(applier func(v int) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v int) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out IntOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, int) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out IntOutput) ApplyURN(applier func(v int) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v int) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out IntOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, int) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out IntOutput) ApplyUint(applier func(v int) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v int) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out IntOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, int) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out IntOutput) ApplyUint16(applier func(v int) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v int) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out IntOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, int) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out IntOutput) ApplyUint32(applier func(v int) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v int) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out IntOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, int) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out IntOutput) ApplyUint64(applier func(v int) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v int) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out IntOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, int) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out IntOutput) ApplyUint8(applier func(v int) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v int) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out IntOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, int) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, intType).(int))
	}))
}

var int16Type = reflect.TypeOf((*int16)(nil)).Elem()

// Int16Input is an input type that accepts Int16 and Int16Output values.
type Int16Input interface {
	Input

	// nolint: unused
	isInt16()
}

// Int16 is an input type for int16 values.
type Int16 int16

// ElementType returns the element type of this Input (int16).
func (Int16) ElementType() reflect.Type {
	return int16Type
}

func (Int16) isInt16() {}

// Int16Output is an Output that returns int16 values.
type Int16Output Output

// ElementType returns the element type of this Output (int16).
func (Int16Output) ElementType() reflect.Type {
	return int16Type
}

func (Int16Output) isInt16() {}

// Apply applies a transformation to the int16 value when it is available.
func (out Int16Output) Apply(applier func(int16) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int16) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the int16 value when it is available.
func (out Int16Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int16) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Int16Output) ApplyAny(applier func(v int16) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v int16) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Int16Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, int16) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Int16Output) ApplyArchive(applier func(v int16) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v int16) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Int16Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, int16) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Int16Output) ApplyArray(applier func(v int16) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v int16) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Int16Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, int16) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Int16Output) ApplyAsset(applier func(v int16) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v int16) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Int16Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, int16) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Int16Output) ApplyAssetOrArchive(applier func(v int16) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v int16) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Int16Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, int16) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Int16Output) ApplyBool(applier func(v int16) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v int16) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Int16Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, int16) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Int16Output) ApplyFloat32(applier func(v int16) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v int16) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Int16Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, int16) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Int16Output) ApplyFloat64(applier func(v int16) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v int16) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Int16Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, int16) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Int16Output) ApplyID(applier func(v int16) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v int16) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Int16Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, int16) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Int16Output) ApplyInt(applier func(v int16) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v int16) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Int16Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, int16) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Int16Output) ApplyInt16(applier func(v int16) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v int16) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Int16Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, int16) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Int16Output) ApplyInt32(applier func(v int16) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v int16) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Int16Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, int16) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Int16Output) ApplyInt64(applier func(v int16) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v int16) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Int16Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, int16) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Int16Output) ApplyInt8(applier func(v int16) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v int16) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Int16Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, int16) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Int16Output) ApplyMap(applier func(v int16) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v int16) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Int16Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, int16) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Int16Output) ApplyString(applier func(v int16) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v int16) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Int16Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, int16) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Int16Output) ApplyURN(applier func(v int16) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v int16) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Int16Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, int16) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Int16Output) ApplyUint(applier func(v int16) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v int16) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Int16Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, int16) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Int16Output) ApplyUint16(applier func(v int16) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v int16) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Int16Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, int16) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Int16Output) ApplyUint32(applier func(v int16) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v int16) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Int16Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, int16) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Int16Output) ApplyUint64(applier func(v int16) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v int16) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Int16Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, int16) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Int16Output) ApplyUint8(applier func(v int16) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v int16) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Int16Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, int16) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int16Type).(int16))
	}))
}

var int32Type = reflect.TypeOf((*int32)(nil)).Elem()

// Int32Input is an input type that accepts Int32 and Int32Output values.
type Int32Input interface {
	Input

	// nolint: unused
	isInt32()
}

// Int32 is an input type for int32 values.
type Int32 int32

// ElementType returns the element type of this Input (int32).
func (Int32) ElementType() reflect.Type {
	return int32Type
}

func (Int32) isInt32() {}

// Int32Output is an Output that returns int32 values.
type Int32Output Output

// ElementType returns the element type of this Output (int32).
func (Int32Output) ElementType() reflect.Type {
	return int32Type
}

func (Int32Output) isInt32() {}

// Apply applies a transformation to the int32 value when it is available.
func (out Int32Output) Apply(applier func(int32) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the int32 value when it is available.
func (out Int32Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int32) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Int32Output) ApplyAny(applier func(v int32) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v int32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Int32Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, int32) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Int32Output) ApplyArchive(applier func(v int32) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v int32) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Int32Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, int32) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Int32Output) ApplyArray(applier func(v int32) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v int32) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Int32Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, int32) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Int32Output) ApplyAsset(applier func(v int32) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v int32) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Int32Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, int32) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Int32Output) ApplyAssetOrArchive(applier func(v int32) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v int32) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Int32Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, int32) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Int32Output) ApplyBool(applier func(v int32) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v int32) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Int32Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, int32) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Int32Output) ApplyFloat32(applier func(v int32) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v int32) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Int32Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, int32) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Int32Output) ApplyFloat64(applier func(v int32) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v int32) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Int32Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, int32) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Int32Output) ApplyID(applier func(v int32) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v int32) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Int32Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, int32) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Int32Output) ApplyInt(applier func(v int32) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v int32) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Int32Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, int32) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Int32Output) ApplyInt16(applier func(v int32) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v int32) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Int32Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, int32) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Int32Output) ApplyInt32(applier func(v int32) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v int32) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Int32Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, int32) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Int32Output) ApplyInt64(applier func(v int32) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v int32) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Int32Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, int32) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Int32Output) ApplyInt8(applier func(v int32) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v int32) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Int32Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, int32) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Int32Output) ApplyMap(applier func(v int32) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v int32) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Int32Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, int32) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Int32Output) ApplyString(applier func(v int32) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v int32) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Int32Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, int32) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Int32Output) ApplyURN(applier func(v int32) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v int32) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Int32Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, int32) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Int32Output) ApplyUint(applier func(v int32) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v int32) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Int32Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, int32) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Int32Output) ApplyUint16(applier func(v int32) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v int32) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Int32Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, int32) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Int32Output) ApplyUint32(applier func(v int32) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v int32) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Int32Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, int32) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Int32Output) ApplyUint64(applier func(v int32) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v int32) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Int32Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, int32) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Int32Output) ApplyUint8(applier func(v int32) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v int32) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Int32Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, int32) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int32Type).(int32))
	}))
}

var int64Type = reflect.TypeOf((*int64)(nil)).Elem()

// Int64Input is an input type that accepts Int64 and Int64Output values.
type Int64Input interface {
	Input

	// nolint: unused
	isInt64()
}

// Int64 is an input type for int64 values.
type Int64 int64

// ElementType returns the element type of this Input (int64).
func (Int64) ElementType() reflect.Type {
	return int64Type
}

func (Int64) isInt64() {}

// Int64Output is an Output that returns int64 values.
type Int64Output Output

// ElementType returns the element type of this Output (int64).
func (Int64Output) ElementType() reflect.Type {
	return int64Type
}

func (Int64Output) isInt64() {}

// Apply applies a transformation to the int64 value when it is available.
func (out Int64Output) Apply(applier func(int64) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the int64 value when it is available.
func (out Int64Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int64) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Int64Output) ApplyAny(applier func(v int64) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v int64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Int64Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, int64) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Int64Output) ApplyArchive(applier func(v int64) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v int64) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Int64Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, int64) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Int64Output) ApplyArray(applier func(v int64) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v int64) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Int64Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, int64) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Int64Output) ApplyAsset(applier func(v int64) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v int64) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Int64Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, int64) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Int64Output) ApplyAssetOrArchive(applier func(v int64) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v int64) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Int64Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, int64) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Int64Output) ApplyBool(applier func(v int64) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v int64) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Int64Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, int64) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Int64Output) ApplyFloat32(applier func(v int64) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v int64) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Int64Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, int64) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Int64Output) ApplyFloat64(applier func(v int64) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v int64) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Int64Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, int64) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Int64Output) ApplyID(applier func(v int64) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v int64) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Int64Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, int64) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Int64Output) ApplyInt(applier func(v int64) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v int64) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Int64Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, int64) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Int64Output) ApplyInt16(applier func(v int64) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v int64) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Int64Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, int64) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Int64Output) ApplyInt32(applier func(v int64) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v int64) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Int64Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, int64) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Int64Output) ApplyInt64(applier func(v int64) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v int64) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Int64Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, int64) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Int64Output) ApplyInt8(applier func(v int64) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v int64) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Int64Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, int64) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Int64Output) ApplyMap(applier func(v int64) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v int64) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Int64Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, int64) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Int64Output) ApplyString(applier func(v int64) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v int64) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Int64Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, int64) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Int64Output) ApplyURN(applier func(v int64) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v int64) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Int64Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, int64) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Int64Output) ApplyUint(applier func(v int64) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v int64) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Int64Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, int64) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Int64Output) ApplyUint16(applier func(v int64) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v int64) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Int64Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, int64) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Int64Output) ApplyUint32(applier func(v int64) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v int64) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Int64Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, int64) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Int64Output) ApplyUint64(applier func(v int64) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v int64) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Int64Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, int64) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Int64Output) ApplyUint8(applier func(v int64) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v int64) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Int64Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, int64) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int64Type).(int64))
	}))
}

var int8Type = reflect.TypeOf((*int8)(nil)).Elem()

// Int8Input is an input type that accepts Int8 and Int8Output values.
type Int8Input interface {
	Input

	// nolint: unused
	isInt8()
}

// Int8 is an input type for int8 values.
type Int8 int8

// ElementType returns the element type of this Input (int8).
func (Int8) ElementType() reflect.Type {
	return int8Type
}

func (Int8) isInt8() {}

// Int8Output is an Output that returns int8 values.
type Int8Output Output

// ElementType returns the element type of this Output (int8).
func (Int8Output) ElementType() reflect.Type {
	return int8Type
}

func (Int8Output) isInt8() {}

// Apply applies a transformation to the int8 value when it is available.
func (out Int8Output) Apply(applier func(int8) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v int8) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the int8 value when it is available.
func (out Int8Output) ApplyWithContext(ctx context.Context, applier func(context.Context, int8) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Int8Output) ApplyAny(applier func(v int8) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v int8) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Int8Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, int8) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Int8Output) ApplyArchive(applier func(v int8) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v int8) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Int8Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, int8) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Int8Output) ApplyArray(applier func(v int8) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v int8) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Int8Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, int8) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Int8Output) ApplyAsset(applier func(v int8) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v int8) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Int8Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, int8) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Int8Output) ApplyAssetOrArchive(applier func(v int8) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v int8) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Int8Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, int8) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Int8Output) ApplyBool(applier func(v int8) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v int8) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Int8Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, int8) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Int8Output) ApplyFloat32(applier func(v int8) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v int8) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Int8Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, int8) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Int8Output) ApplyFloat64(applier func(v int8) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v int8) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Int8Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, int8) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Int8Output) ApplyID(applier func(v int8) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v int8) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Int8Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, int8) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Int8Output) ApplyInt(applier func(v int8) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v int8) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Int8Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, int8) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Int8Output) ApplyInt16(applier func(v int8) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v int8) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Int8Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, int8) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Int8Output) ApplyInt32(applier func(v int8) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v int8) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Int8Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, int8) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Int8Output) ApplyInt64(applier func(v int8) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v int8) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Int8Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, int8) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Int8Output) ApplyInt8(applier func(v int8) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v int8) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Int8Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, int8) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Int8Output) ApplyMap(applier func(v int8) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v int8) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Int8Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, int8) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Int8Output) ApplyString(applier func(v int8) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v int8) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Int8Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, int8) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Int8Output) ApplyURN(applier func(v int8) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v int8) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Int8Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, int8) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Int8Output) ApplyUint(applier func(v int8) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v int8) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Int8Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, int8) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Int8Output) ApplyUint16(applier func(v int8) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v int8) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Int8Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, int8) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Int8Output) ApplyUint32(applier func(v int8) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v int8) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Int8Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, int8) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Int8Output) ApplyUint64(applier func(v int8) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v int8) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Int8Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, int8) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Int8Output) ApplyUint8(applier func(v int8) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v int8) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Int8Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, int8) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, int8Type).(int8))
	}))
}

var mapType = reflect.TypeOf((*map[string]interface{})(nil)).Elem()

// MapInput is an input type that accepts Map and MapOutput values.
type MapInput interface {
	Input

	// nolint: unused
	isMap()
}

// Map is an input type for map[string]interface{} values.
type Map map[string]interface{}

// ElementType returns the element type of this Input (map[string]interface{}).
func (Map) ElementType() reflect.Type {
	return mapType
}

func (Map) isMap() {}

// MapOutput is an Output that returns map[string]interface{} values.
type MapOutput Output

// ElementType returns the element type of this Output (map[string]interface{}).
func (MapOutput) ElementType() reflect.Type {
	return mapType
}

func (MapOutput) isMap() {}

// Apply applies a transformation to the map value when it is available.
func (out MapOutput) Apply(applier func(map[string]interface{}) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the map value when it is available.
func (out MapOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out MapOutput) ApplyAny(applier func(v map[string]interface{}) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out MapOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out MapOutput) ApplyArchive(applier func(v map[string]interface{}) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out MapOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out MapOutput) ApplyArray(applier func(v map[string]interface{}) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out MapOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out MapOutput) ApplyAsset(applier func(v map[string]interface{}) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out MapOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out MapOutput) ApplyAssetOrArchive(applier func(v map[string]interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out MapOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out MapOutput) ApplyBool(applier func(v map[string]interface{}) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out MapOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out MapOutput) ApplyFloat32(applier func(v map[string]interface{}) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out MapOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out MapOutput) ApplyFloat64(applier func(v map[string]interface{}) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out MapOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out MapOutput) ApplyID(applier func(v map[string]interface{}) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out MapOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out MapOutput) ApplyInt(applier func(v map[string]interface{}) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out MapOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out MapOutput) ApplyInt16(applier func(v map[string]interface{}) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out MapOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out MapOutput) ApplyInt32(applier func(v map[string]interface{}) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out MapOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out MapOutput) ApplyInt64(applier func(v map[string]interface{}) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out MapOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out MapOutput) ApplyInt8(applier func(v map[string]interface{}) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out MapOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out MapOutput) ApplyMap(applier func(v map[string]interface{}) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out MapOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out MapOutput) ApplyString(applier func(v map[string]interface{}) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out MapOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out MapOutput) ApplyURN(applier func(v map[string]interface{}) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out MapOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out MapOutput) ApplyUint(applier func(v map[string]interface{}) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out MapOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out MapOutput) ApplyUint16(applier func(v map[string]interface{}) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out MapOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out MapOutput) ApplyUint32(applier func(v map[string]interface{}) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out MapOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out MapOutput) ApplyUint64(applier func(v map[string]interface{}) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out MapOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out MapOutput) ApplyUint8(applier func(v map[string]interface{}) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v map[string]interface{}) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out MapOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, map[string]interface{}) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, mapType).(map[string]interface{}))
	}))
}

var stringType = reflect.TypeOf((*string)(nil)).Elem()

// StringInput is an input type that accepts String and StringOutput values.
type StringInput interface {
	Input

	// nolint: unused
	isString()
}

// String is an input type for string values.
type String string

// ElementType returns the element type of this Input (string).
func (String) ElementType() reflect.Type {
	return stringType
}

func (String) isString() {}

// StringOutput is an Output that returns string values.
type StringOutput Output

// ElementType returns the element type of this Output (string).
func (StringOutput) ElementType() reflect.Type {
	return stringType
}

func (StringOutput) isString() {}

// Apply applies a transformation to the string value when it is available.
func (out StringOutput) Apply(applier func(string) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v string) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the string value when it is available.
func (out StringOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, string) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out StringOutput) ApplyAny(applier func(v string) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v string) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out StringOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, string) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out StringOutput) ApplyArchive(applier func(v string) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v string) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out StringOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, string) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out StringOutput) ApplyArray(applier func(v string) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v string) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out StringOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, string) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out StringOutput) ApplyAsset(applier func(v string) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v string) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out StringOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, string) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out StringOutput) ApplyAssetOrArchive(applier func(v string) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v string) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out StringOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, string) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out StringOutput) ApplyBool(applier func(v string) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v string) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out StringOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, string) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out StringOutput) ApplyFloat32(applier func(v string) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v string) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out StringOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, string) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out StringOutput) ApplyFloat64(applier func(v string) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v string) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out StringOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, string) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out StringOutput) ApplyID(applier func(v string) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v string) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out StringOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, string) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out StringOutput) ApplyInt(applier func(v string) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v string) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out StringOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, string) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out StringOutput) ApplyInt16(applier func(v string) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v string) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out StringOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, string) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out StringOutput) ApplyInt32(applier func(v string) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v string) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out StringOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, string) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out StringOutput) ApplyInt64(applier func(v string) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v string) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out StringOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, string) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out StringOutput) ApplyInt8(applier func(v string) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v string) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out StringOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, string) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out StringOutput) ApplyMap(applier func(v string) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v string) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out StringOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, string) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out StringOutput) ApplyString(applier func(v string) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v string) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out StringOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, string) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out StringOutput) ApplyURN(applier func(v string) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v string) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out StringOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, string) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out StringOutput) ApplyUint(applier func(v string) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v string) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out StringOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, string) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out StringOutput) ApplyUint16(applier func(v string) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v string) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out StringOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, string) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out StringOutput) ApplyUint32(applier func(v string) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v string) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out StringOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, string) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out StringOutput) ApplyUint64(applier func(v string) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v string) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out StringOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, string) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out StringOutput) ApplyUint8(applier func(v string) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v string) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out StringOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, string) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, stringType).(string))
	}))
}

var urnType = reflect.TypeOf((*URN)(nil)).Elem()

// URNInput is an input type that accepts URN and URNOutput values.
type URNInput interface {
	Input

	// nolint: unused
	isURN()
}

// ElementType returns the element type of this Input (URN).
func (URN) ElementType() reflect.Type {
	return urnType
}

func (URN) isURN() {}

func (URN) isString() {}

// URNOutput is an Output that returns URN values.
type URNOutput Output

// ElementType returns the element type of this Output (URN).
func (URNOutput) ElementType() reflect.Type {
	return urnType
}

func (URNOutput) isURN() {}

func (URNOutput) isString() {}

// Apply applies a transformation to the urn value when it is available.
func (out URNOutput) Apply(applier func(URN) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v URN) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the urn value when it is available.
func (out URNOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, URN) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out URNOutput) ApplyAny(applier func(v URN) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v URN) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out URNOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, URN) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out URNOutput) ApplyArchive(applier func(v URN) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v URN) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out URNOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, URN) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out URNOutput) ApplyArray(applier func(v URN) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v URN) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out URNOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, URN) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out URNOutput) ApplyAsset(applier func(v URN) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v URN) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out URNOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, URN) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out URNOutput) ApplyAssetOrArchive(applier func(v URN) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v URN) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out URNOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, URN) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out URNOutput) ApplyBool(applier func(v URN) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v URN) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out URNOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, URN) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out URNOutput) ApplyFloat32(applier func(v URN) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v URN) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out URNOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, URN) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out URNOutput) ApplyFloat64(applier func(v URN) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v URN) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out URNOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, URN) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out URNOutput) ApplyID(applier func(v URN) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v URN) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out URNOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, URN) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out URNOutput) ApplyInt(applier func(v URN) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v URN) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out URNOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, URN) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out URNOutput) ApplyInt16(applier func(v URN) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v URN) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out URNOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, URN) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out URNOutput) ApplyInt32(applier func(v URN) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v URN) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out URNOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, URN) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out URNOutput) ApplyInt64(applier func(v URN) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v URN) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out URNOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, URN) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out URNOutput) ApplyInt8(applier func(v URN) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v URN) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out URNOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, URN) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out URNOutput) ApplyMap(applier func(v URN) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v URN) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out URNOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, URN) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out URNOutput) ApplyString(applier func(v URN) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v URN) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out URNOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, URN) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out URNOutput) ApplyURN(applier func(v URN) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v URN) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out URNOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, URN) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out URNOutput) ApplyUint(applier func(v URN) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v URN) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out URNOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, URN) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out URNOutput) ApplyUint16(applier func(v URN) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v URN) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out URNOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, URN) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out URNOutput) ApplyUint32(applier func(v URN) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v URN) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out URNOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, URN) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out URNOutput) ApplyUint64(applier func(v URN) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v URN) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out URNOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, URN) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out URNOutput) ApplyUint8(applier func(v URN) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v URN) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out URNOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, URN) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, urnType).(URN))
	}))
}

var uintType = reflect.TypeOf((*uint)(nil)).Elem()

// UintInput is an input type that accepts Uint and UintOutput values.
type UintInput interface {
	Input

	// nolint: unused
	isUint()
}

// Uint is an input type for uint values.
type Uint uint

// ElementType returns the element type of this Input (uint).
func (Uint) ElementType() reflect.Type {
	return uintType
}

func (Uint) isUint() {}

// UintOutput is an Output that returns uint values.
type UintOutput Output

// ElementType returns the element type of this Output (uint).
func (UintOutput) ElementType() reflect.Type {
	return uintType
}

func (UintOutput) isUint() {}

// Apply applies a transformation to the uint value when it is available.
func (out UintOutput) Apply(applier func(uint) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the uint value when it is available.
func (out UintOutput) ApplyWithContext(ctx context.Context, applier func(context.Context, uint) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out UintOutput) ApplyAny(applier func(v uint) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v uint) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out UintOutput) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, uint) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out UintOutput) ApplyArchive(applier func(v uint) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v uint) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out UintOutput) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, uint) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out UintOutput) ApplyArray(applier func(v uint) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v uint) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out UintOutput) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, uint) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out UintOutput) ApplyAsset(applier func(v uint) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v uint) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out UintOutput) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, uint) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out UintOutput) ApplyAssetOrArchive(applier func(v uint) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v uint) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out UintOutput) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, uint) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out UintOutput) ApplyBool(applier func(v uint) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v uint) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out UintOutput) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, uint) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out UintOutput) ApplyFloat32(applier func(v uint) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v uint) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out UintOutput) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, uint) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out UintOutput) ApplyFloat64(applier func(v uint) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v uint) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out UintOutput) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, uint) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out UintOutput) ApplyID(applier func(v uint) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v uint) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out UintOutput) ApplyIDWithContext(ctx context.Context, applier func(context.Context, uint) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out UintOutput) ApplyInt(applier func(v uint) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v uint) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out UintOutput) ApplyIntWithContext(ctx context.Context, applier func(context.Context, uint) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out UintOutput) ApplyInt16(applier func(v uint) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v uint) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out UintOutput) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, uint) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out UintOutput) ApplyInt32(applier func(v uint) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v uint) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out UintOutput) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, uint) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out UintOutput) ApplyInt64(applier func(v uint) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v uint) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out UintOutput) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, uint) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out UintOutput) ApplyInt8(applier func(v uint) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v uint) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out UintOutput) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, uint) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out UintOutput) ApplyMap(applier func(v uint) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v uint) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out UintOutput) ApplyMapWithContext(ctx context.Context, applier func(context.Context, uint) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out UintOutput) ApplyString(applier func(v uint) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v uint) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out UintOutput) ApplyStringWithContext(ctx context.Context, applier func(context.Context, uint) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out UintOutput) ApplyURN(applier func(v uint) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v uint) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out UintOutput) ApplyURNWithContext(ctx context.Context, applier func(context.Context, uint) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out UintOutput) ApplyUint(applier func(v uint) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v uint) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out UintOutput) ApplyUintWithContext(ctx context.Context, applier func(context.Context, uint) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out UintOutput) ApplyUint16(applier func(v uint) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v uint) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out UintOutput) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, uint) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out UintOutput) ApplyUint32(applier func(v uint) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v uint) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out UintOutput) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, uint) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out UintOutput) ApplyUint64(applier func(v uint) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v uint) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out UintOutput) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, uint) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out UintOutput) ApplyUint8(applier func(v uint) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v uint) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out UintOutput) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, uint) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uintType).(uint))
	}))
}

var uint16Type = reflect.TypeOf((*uint16)(nil)).Elem()

// Uint16Input is an input type that accepts Uint16 and Uint16Output values.
type Uint16Input interface {
	Input

	// nolint: unused
	isUint16()
}

// Uint16 is an input type for uint16 values.
type Uint16 uint16

// ElementType returns the element type of this Input (uint16).
func (Uint16) ElementType() reflect.Type {
	return uint16Type
}

func (Uint16) isUint16() {}

// Uint16Output is an Output that returns uint16 values.
type Uint16Output Output

// ElementType returns the element type of this Output (uint16).
func (Uint16Output) ElementType() reflect.Type {
	return uint16Type
}

func (Uint16Output) isUint16() {}

// Apply applies a transformation to the uint16 value when it is available.
func (out Uint16Output) Apply(applier func(uint16) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint16) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the uint16 value when it is available.
func (out Uint16Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint16) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Uint16Output) ApplyAny(applier func(v uint16) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v uint16) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Uint16Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, uint16) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Uint16Output) ApplyArchive(applier func(v uint16) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v uint16) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Uint16Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, uint16) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Uint16Output) ApplyArray(applier func(v uint16) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v uint16) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Uint16Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, uint16) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Uint16Output) ApplyAsset(applier func(v uint16) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v uint16) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Uint16Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, uint16) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Uint16Output) ApplyAssetOrArchive(applier func(v uint16) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v uint16) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Uint16Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, uint16) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Uint16Output) ApplyBool(applier func(v uint16) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v uint16) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Uint16Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, uint16) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Uint16Output) ApplyFloat32(applier func(v uint16) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v uint16) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Uint16Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, uint16) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Uint16Output) ApplyFloat64(applier func(v uint16) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v uint16) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Uint16Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, uint16) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Uint16Output) ApplyID(applier func(v uint16) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v uint16) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Uint16Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, uint16) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Uint16Output) ApplyInt(applier func(v uint16) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v uint16) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Uint16Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, uint16) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Uint16Output) ApplyInt16(applier func(v uint16) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v uint16) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Uint16Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, uint16) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Uint16Output) ApplyInt32(applier func(v uint16) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v uint16) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Uint16Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, uint16) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Uint16Output) ApplyInt64(applier func(v uint16) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v uint16) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Uint16Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, uint16) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Uint16Output) ApplyInt8(applier func(v uint16) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v uint16) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Uint16Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, uint16) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Uint16Output) ApplyMap(applier func(v uint16) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v uint16) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Uint16Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, uint16) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Uint16Output) ApplyString(applier func(v uint16) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v uint16) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Uint16Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, uint16) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Uint16Output) ApplyURN(applier func(v uint16) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v uint16) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Uint16Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, uint16) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Uint16Output) ApplyUint(applier func(v uint16) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v uint16) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Uint16Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, uint16) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Uint16Output) ApplyUint16(applier func(v uint16) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v uint16) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Uint16Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, uint16) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Uint16Output) ApplyUint32(applier func(v uint16) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v uint16) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Uint16Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, uint16) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Uint16Output) ApplyUint64(applier func(v uint16) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v uint16) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Uint16Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, uint16) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Uint16Output) ApplyUint8(applier func(v uint16) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v uint16) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Uint16Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, uint16) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint16Type).(uint16))
	}))
}

var uint32Type = reflect.TypeOf((*uint32)(nil)).Elem()

// Uint32Input is an input type that accepts Uint32 and Uint32Output values.
type Uint32Input interface {
	Input

	// nolint: unused
	isUint32()
}

// Uint32 is an input type for uint32 values.
type Uint32 uint32

// ElementType returns the element type of this Input (uint32).
func (Uint32) ElementType() reflect.Type {
	return uint32Type
}

func (Uint32) isUint32() {}

// Uint32Output is an Output that returns uint32 values.
type Uint32Output Output

// ElementType returns the element type of this Output (uint32).
func (Uint32Output) ElementType() reflect.Type {
	return uint32Type
}

func (Uint32Output) isUint32() {}

// Apply applies a transformation to the uint32 value when it is available.
func (out Uint32Output) Apply(applier func(uint32) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the uint32 value when it is available.
func (out Uint32Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint32) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Uint32Output) ApplyAny(applier func(v uint32) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v uint32) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Uint32Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, uint32) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Uint32Output) ApplyArchive(applier func(v uint32) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v uint32) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Uint32Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, uint32) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Uint32Output) ApplyArray(applier func(v uint32) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v uint32) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Uint32Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, uint32) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Uint32Output) ApplyAsset(applier func(v uint32) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v uint32) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Uint32Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, uint32) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Uint32Output) ApplyAssetOrArchive(applier func(v uint32) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v uint32) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Uint32Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, uint32) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Uint32Output) ApplyBool(applier func(v uint32) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v uint32) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Uint32Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, uint32) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Uint32Output) ApplyFloat32(applier func(v uint32) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v uint32) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Uint32Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, uint32) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Uint32Output) ApplyFloat64(applier func(v uint32) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v uint32) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Uint32Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, uint32) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Uint32Output) ApplyID(applier func(v uint32) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v uint32) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Uint32Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, uint32) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Uint32Output) ApplyInt(applier func(v uint32) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v uint32) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Uint32Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, uint32) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Uint32Output) ApplyInt16(applier func(v uint32) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v uint32) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Uint32Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, uint32) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Uint32Output) ApplyInt32(applier func(v uint32) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v uint32) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Uint32Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, uint32) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Uint32Output) ApplyInt64(applier func(v uint32) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v uint32) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Uint32Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, uint32) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Uint32Output) ApplyInt8(applier func(v uint32) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v uint32) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Uint32Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, uint32) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Uint32Output) ApplyMap(applier func(v uint32) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v uint32) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Uint32Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, uint32) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Uint32Output) ApplyString(applier func(v uint32) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v uint32) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Uint32Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, uint32) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Uint32Output) ApplyURN(applier func(v uint32) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v uint32) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Uint32Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, uint32) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Uint32Output) ApplyUint(applier func(v uint32) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v uint32) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Uint32Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, uint32) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Uint32Output) ApplyUint16(applier func(v uint32) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v uint32) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Uint32Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, uint32) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Uint32Output) ApplyUint32(applier func(v uint32) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v uint32) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Uint32Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, uint32) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Uint32Output) ApplyUint64(applier func(v uint32) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v uint32) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Uint32Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, uint32) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Uint32Output) ApplyUint8(applier func(v uint32) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v uint32) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Uint32Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, uint32) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint32Type).(uint32))
	}))
}

var uint64Type = reflect.TypeOf((*uint64)(nil)).Elem()

// Uint64Input is an input type that accepts Uint64 and Uint64Output values.
type Uint64Input interface {
	Input

	// nolint: unused
	isUint64()
}

// Uint64 is an input type for uint64 values.
type Uint64 uint64

// ElementType returns the element type of this Input (uint64).
func (Uint64) ElementType() reflect.Type {
	return uint64Type
}

func (Uint64) isUint64() {}

// Uint64Output is an Output that returns uint64 values.
type Uint64Output Output

// ElementType returns the element type of this Output (uint64).
func (Uint64Output) ElementType() reflect.Type {
	return uint64Type
}

func (Uint64Output) isUint64() {}

// Apply applies a transformation to the uint64 value when it is available.
func (out Uint64Output) Apply(applier func(uint64) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the uint64 value when it is available.
func (out Uint64Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint64) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Uint64Output) ApplyAny(applier func(v uint64) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v uint64) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Uint64Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, uint64) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Uint64Output) ApplyArchive(applier func(v uint64) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v uint64) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Uint64Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, uint64) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Uint64Output) ApplyArray(applier func(v uint64) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v uint64) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Uint64Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, uint64) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Uint64Output) ApplyAsset(applier func(v uint64) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v uint64) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Uint64Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, uint64) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Uint64Output) ApplyAssetOrArchive(applier func(v uint64) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v uint64) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Uint64Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, uint64) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Uint64Output) ApplyBool(applier func(v uint64) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v uint64) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Uint64Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, uint64) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Uint64Output) ApplyFloat32(applier func(v uint64) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v uint64) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Uint64Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, uint64) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Uint64Output) ApplyFloat64(applier func(v uint64) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v uint64) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Uint64Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, uint64) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Uint64Output) ApplyID(applier func(v uint64) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v uint64) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Uint64Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, uint64) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Uint64Output) ApplyInt(applier func(v uint64) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v uint64) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Uint64Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, uint64) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Uint64Output) ApplyInt16(applier func(v uint64) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v uint64) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Uint64Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, uint64) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Uint64Output) ApplyInt32(applier func(v uint64) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v uint64) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Uint64Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, uint64) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Uint64Output) ApplyInt64(applier func(v uint64) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v uint64) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Uint64Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, uint64) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Uint64Output) ApplyInt8(applier func(v uint64) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v uint64) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Uint64Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, uint64) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Uint64Output) ApplyMap(applier func(v uint64) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v uint64) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Uint64Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, uint64) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Uint64Output) ApplyString(applier func(v uint64) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v uint64) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Uint64Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, uint64) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Uint64Output) ApplyURN(applier func(v uint64) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v uint64) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Uint64Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, uint64) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Uint64Output) ApplyUint(applier func(v uint64) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v uint64) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Uint64Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, uint64) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Uint64Output) ApplyUint16(applier func(v uint64) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v uint64) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Uint64Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, uint64) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Uint64Output) ApplyUint32(applier func(v uint64) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v uint64) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Uint64Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, uint64) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Uint64Output) ApplyUint64(applier func(v uint64) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v uint64) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Uint64Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, uint64) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Uint64Output) ApplyUint8(applier func(v uint64) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v uint64) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Uint64Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, uint64) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint64Type).(uint64))
	}))
}

var uint8Type = reflect.TypeOf((*uint8)(nil)).Elem()

// Uint8Input is an input type that accepts Uint8 and Uint8Output values.
type Uint8Input interface {
	Input

	// nolint: unused
	isUint8()
}

// Uint8 is an input type for uint8 values.
type Uint8 uint8

// ElementType returns the element type of this Input (uint8).
func (Uint8) ElementType() reflect.Type {
	return uint8Type
}

func (Uint8) isUint8() {}

// Uint8Output is an Output that returns uint8 values.
type Uint8Output Output

// ElementType returns the element type of this Output (uint8).
func (Uint8Output) ElementType() reflect.Type {
	return uint8Type
}

func (Uint8Output) isUint8() {}

// Apply applies a transformation to the uint8 value when it is available.
func (out Uint8Output) Apply(applier func(uint8) (interface{}, error)) Output {
	return out.ApplyWithContext(context.Background(), func(_ context.Context, v uint8) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext applies a transformation to the uint8 value when it is available.
func (out Uint8Output) ApplyWithContext(ctx context.Context, applier func(context.Context, uint8) (interface{}, error)) Output {
	return Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	})
}

// ApplyAny is like Apply, but returns a AnyOutput.
func (out Uint8Output) ApplyAny(applier func(v uint8) (interface{}, error)) AnyOutput {
	return out.ApplyAnyWithContext(context.Background(), func(_ context.Context, v uint8) (interface{}, error) {
		return applier(v)
	})
}

// ApplyAnyWithContext is like ApplyWithContext, but returns a AnyOutput.
func (out Uint8Output) ApplyAnyWithContext(ctx context.Context, applier func(context.Context, uint8) (interface{}, error)) AnyOutput {
	return AnyOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyArchive is like Apply, but returns a ArchiveOutput.
func (out Uint8Output) ApplyArchive(applier func(v uint8) (Archive, error)) ArchiveOutput {
	return out.ApplyArchiveWithContext(context.Background(), func(_ context.Context, v uint8) (Archive, error) {
		return applier(v)
	})
}

// ApplyArchiveWithContext is like ApplyWithContext, but returns a ArchiveOutput.
func (out Uint8Output) ApplyArchiveWithContext(ctx context.Context, applier func(context.Context, uint8) (Archive, error)) ArchiveOutput {
	return ArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyArray is like Apply, but returns a ArrayOutput.
func (out Uint8Output) ApplyArray(applier func(v uint8) ([]interface{}, error)) ArrayOutput {
	return out.ApplyArrayWithContext(context.Background(), func(_ context.Context, v uint8) ([]interface{}, error) {
		return applier(v)
	})
}

// ApplyArrayWithContext is like ApplyWithContext, but returns a ArrayOutput.
func (out Uint8Output) ApplyArrayWithContext(ctx context.Context, applier func(context.Context, uint8) ([]interface{}, error)) ArrayOutput {
	return ArrayOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyAsset is like Apply, but returns a AssetOutput.
func (out Uint8Output) ApplyAsset(applier func(v uint8) (Asset, error)) AssetOutput {
	return out.ApplyAssetWithContext(context.Background(), func(_ context.Context, v uint8) (Asset, error) {
		return applier(v)
	})
}

// ApplyAssetWithContext is like ApplyWithContext, but returns a AssetOutput.
func (out Uint8Output) ApplyAssetWithContext(ctx context.Context, applier func(context.Context, uint8) (Asset, error)) AssetOutput {
	return AssetOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyAssetOrArchive is like Apply, but returns a AssetOrArchiveOutput.
func (out Uint8Output) ApplyAssetOrArchive(applier func(v uint8) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return out.ApplyAssetOrArchiveWithContext(context.Background(), func(_ context.Context, v uint8) (AssetOrArchive, error) {
		return applier(v)
	})
}

// ApplyAssetOrArchiveWithContext is like ApplyWithContext, but returns a AssetOrArchiveOutput.
func (out Uint8Output) ApplyAssetOrArchiveWithContext(ctx context.Context, applier func(context.Context, uint8) (AssetOrArchive, error)) AssetOrArchiveOutput {
	return AssetOrArchiveOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyBool is like Apply, but returns a BoolOutput.
func (out Uint8Output) ApplyBool(applier func(v uint8) (bool, error)) BoolOutput {
	return out.ApplyBoolWithContext(context.Background(), func(_ context.Context, v uint8) (bool, error) {
		return applier(v)
	})
}

// ApplyBoolWithContext is like ApplyWithContext, but returns a BoolOutput.
func (out Uint8Output) ApplyBoolWithContext(ctx context.Context, applier func(context.Context, uint8) (bool, error)) BoolOutput {
	return BoolOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyFloat32 is like Apply, but returns a Float32Output.
func (out Uint8Output) ApplyFloat32(applier func(v uint8) (float32, error)) Float32Output {
	return out.ApplyFloat32WithContext(context.Background(), func(_ context.Context, v uint8) (float32, error) {
		return applier(v)
	})
}

// ApplyFloat32WithContext is like ApplyWithContext, but returns a Float32Output.
func (out Uint8Output) ApplyFloat32WithContext(ctx context.Context, applier func(context.Context, uint8) (float32, error)) Float32Output {
	return Float32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyFloat64 is like Apply, but returns a Float64Output.
func (out Uint8Output) ApplyFloat64(applier func(v uint8) (float64, error)) Float64Output {
	return out.ApplyFloat64WithContext(context.Background(), func(_ context.Context, v uint8) (float64, error) {
		return applier(v)
	})
}

// ApplyFloat64WithContext is like ApplyWithContext, but returns a Float64Output.
func (out Uint8Output) ApplyFloat64WithContext(ctx context.Context, applier func(context.Context, uint8) (float64, error)) Float64Output {
	return Float64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyID is like Apply, but returns a IDOutput.
func (out Uint8Output) ApplyID(applier func(v uint8) (ID, error)) IDOutput {
	return out.ApplyIDWithContext(context.Background(), func(_ context.Context, v uint8) (ID, error) {
		return applier(v)
	})
}

// ApplyIDWithContext is like ApplyWithContext, but returns a IDOutput.
func (out Uint8Output) ApplyIDWithContext(ctx context.Context, applier func(context.Context, uint8) (ID, error)) IDOutput {
	return IDOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyInt is like Apply, but returns a IntOutput.
func (out Uint8Output) ApplyInt(applier func(v uint8) (int, error)) IntOutput {
	return out.ApplyIntWithContext(context.Background(), func(_ context.Context, v uint8) (int, error) {
		return applier(v)
	})
}

// ApplyIntWithContext is like ApplyWithContext, but returns a IntOutput.
func (out Uint8Output) ApplyIntWithContext(ctx context.Context, applier func(context.Context, uint8) (int, error)) IntOutput {
	return IntOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyInt16 is like Apply, but returns a Int16Output.
func (out Uint8Output) ApplyInt16(applier func(v uint8) (int16, error)) Int16Output {
	return out.ApplyInt16WithContext(context.Background(), func(_ context.Context, v uint8) (int16, error) {
		return applier(v)
	})
}

// ApplyInt16WithContext is like ApplyWithContext, but returns a Int16Output.
func (out Uint8Output) ApplyInt16WithContext(ctx context.Context, applier func(context.Context, uint8) (int16, error)) Int16Output {
	return Int16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyInt32 is like Apply, but returns a Int32Output.
func (out Uint8Output) ApplyInt32(applier func(v uint8) (int32, error)) Int32Output {
	return out.ApplyInt32WithContext(context.Background(), func(_ context.Context, v uint8) (int32, error) {
		return applier(v)
	})
}

// ApplyInt32WithContext is like ApplyWithContext, but returns a Int32Output.
func (out Uint8Output) ApplyInt32WithContext(ctx context.Context, applier func(context.Context, uint8) (int32, error)) Int32Output {
	return Int32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyInt64 is like Apply, but returns a Int64Output.
func (out Uint8Output) ApplyInt64(applier func(v uint8) (int64, error)) Int64Output {
	return out.ApplyInt64WithContext(context.Background(), func(_ context.Context, v uint8) (int64, error) {
		return applier(v)
	})
}

// ApplyInt64WithContext is like ApplyWithContext, but returns a Int64Output.
func (out Uint8Output) ApplyInt64WithContext(ctx context.Context, applier func(context.Context, uint8) (int64, error)) Int64Output {
	return Int64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyInt8 is like Apply, but returns a Int8Output.
func (out Uint8Output) ApplyInt8(applier func(v uint8) (int8, error)) Int8Output {
	return out.ApplyInt8WithContext(context.Background(), func(_ context.Context, v uint8) (int8, error) {
		return applier(v)
	})
}

// ApplyInt8WithContext is like ApplyWithContext, but returns a Int8Output.
func (out Uint8Output) ApplyInt8WithContext(ctx context.Context, applier func(context.Context, uint8) (int8, error)) Int8Output {
	return Int8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyMap is like Apply, but returns a MapOutput.
func (out Uint8Output) ApplyMap(applier func(v uint8) (map[string]interface{}, error)) MapOutput {
	return out.ApplyMapWithContext(context.Background(), func(_ context.Context, v uint8) (map[string]interface{}, error) {
		return applier(v)
	})
}

// ApplyMapWithContext is like ApplyWithContext, but returns a MapOutput.
func (out Uint8Output) ApplyMapWithContext(ctx context.Context, applier func(context.Context, uint8) (map[string]interface{}, error)) MapOutput {
	return MapOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyString is like Apply, but returns a StringOutput.
func (out Uint8Output) ApplyString(applier func(v uint8) (string, error)) StringOutput {
	return out.ApplyStringWithContext(context.Background(), func(_ context.Context, v uint8) (string, error) {
		return applier(v)
	})
}

// ApplyStringWithContext is like ApplyWithContext, but returns a StringOutput.
func (out Uint8Output) ApplyStringWithContext(ctx context.Context, applier func(context.Context, uint8) (string, error)) StringOutput {
	return StringOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyURN is like Apply, but returns a URNOutput.
func (out Uint8Output) ApplyURN(applier func(v uint8) (URN, error)) URNOutput {
	return out.ApplyURNWithContext(context.Background(), func(_ context.Context, v uint8) (URN, error) {
		return applier(v)
	})
}

// ApplyURNWithContext is like ApplyWithContext, but returns a URNOutput.
func (out Uint8Output) ApplyURNWithContext(ctx context.Context, applier func(context.Context, uint8) (URN, error)) URNOutput {
	return URNOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyUint is like Apply, but returns a UintOutput.
func (out Uint8Output) ApplyUint(applier func(v uint8) (uint, error)) UintOutput {
	return out.ApplyUintWithContext(context.Background(), func(_ context.Context, v uint8) (uint, error) {
		return applier(v)
	})
}

// ApplyUintWithContext is like ApplyWithContext, but returns a UintOutput.
func (out Uint8Output) ApplyUintWithContext(ctx context.Context, applier func(context.Context, uint8) (uint, error)) UintOutput {
	return UintOutput(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyUint16 is like Apply, but returns a Uint16Output.
func (out Uint8Output) ApplyUint16(applier func(v uint8) (uint16, error)) Uint16Output {
	return out.ApplyUint16WithContext(context.Background(), func(_ context.Context, v uint8) (uint16, error) {
		return applier(v)
	})
}

// ApplyUint16WithContext is like ApplyWithContext, but returns a Uint16Output.
func (out Uint8Output) ApplyUint16WithContext(ctx context.Context, applier func(context.Context, uint8) (uint16, error)) Uint16Output {
	return Uint16Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyUint32 is like Apply, but returns a Uint32Output.
func (out Uint8Output) ApplyUint32(applier func(v uint8) (uint32, error)) Uint32Output {
	return out.ApplyUint32WithContext(context.Background(), func(_ context.Context, v uint8) (uint32, error) {
		return applier(v)
	})
}

// ApplyUint32WithContext is like ApplyWithContext, but returns a Uint32Output.
func (out Uint8Output) ApplyUint32WithContext(ctx context.Context, applier func(context.Context, uint8) (uint32, error)) Uint32Output {
	return Uint32Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyUint64 is like Apply, but returns a Uint64Output.
func (out Uint8Output) ApplyUint64(applier func(v uint8) (uint64, error)) Uint64Output {
	return out.ApplyUint64WithContext(context.Background(), func(_ context.Context, v uint8) (uint64, error) {
		return applier(v)
	})
}

// ApplyUint64WithContext is like ApplyWithContext, but returns a Uint64Output.
func (out Uint8Output) ApplyUint64WithContext(ctx context.Context, applier func(context.Context, uint8) (uint64, error)) Uint64Output {
	return Uint64Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

// ApplyUint8 is like Apply, but returns a Uint8Output.
func (out Uint8Output) ApplyUint8(applier func(v uint8) (uint8, error)) Uint8Output {
	return out.ApplyUint8WithContext(context.Background(), func(_ context.Context, v uint8) (uint8, error) {
		return applier(v)
	})
}

// ApplyUint8WithContext is like ApplyWithContext, but returns a Uint8Output.
func (out Uint8Output) ApplyUint8WithContext(ctx context.Context, applier func(context.Context, uint8) (uint8, error)) Uint8Output {
	return Uint8Output(Output(out).ApplyWithContext(ctx, func(ctx context.Context, v interface{}) (interface{}, error) {
		return applier(ctx, convert(v, uint8Type).(uint8))
	}))
}

func (out IDOutput) await(ctx context.Context) (ID, bool, error) {
	id, known, err := Output(out).await(ctx)
	if !known || err != nil {
		return "", known, err
	}
	return ID(convert(id, stringType).(string)), true, nil
}

func (out URNOutput) await(ctx context.Context) (URN, bool, error) {
	id, known, err := Output(out).await(ctx)
	if !known || err != nil {
		return "", known, err
	}
	return URN(convert(id, stringType).(string)), true, nil
}

func convert(v interface{}, to reflect.Type) interface{} {
	rv := reflect.ValueOf(v)
	if !rv.Type().ConvertibleTo(to) {
		panic(errors.Errorf("cannot convert output value of type %s to %s", rv.Type(), to))
	}
	return rv.Convert(to).Interface()
}
