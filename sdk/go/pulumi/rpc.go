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

package pulumi

import (
	"reflect"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/util/contract"
)

// marshalInputs turns resource property inputs into a map suitable for marshaling.
func marshalInputs(props map[string]Input) (resource.PropertyMap, map[string][]URN, []URN, error) {
	var depURNs []URN
	depset := map[URN]bool{}
	pmap, pdeps := resource.PropertyMap{}, map[string][]URN{}
	for key := range props {
		// Get the underlying value, possibly waiting for an output to arrive.
		v, resourceDeps, err := marshalInput(props[key], true)
		if err != nil {
			return nil, nil, nil, errors.Wrapf(err, "awaiting input property %s", key)
		}

		pmap[resource.PropertyKey(key)] = v

		// Record all dependencies accumulated from reading this property.
		var deps []URN
		pdepset := map[URN]bool{}
		for _, dep := range resourceDeps {
			depURN, _, err := dep.GetURN().await(context.TODO())
			if err != nil {
				return nil, nil, nil, err
			}
			if !pdepset[depURN] {
				deps = append(deps, depURN)
				pdepset[depURN] = true
			}
			if !depset[depURN] {
				depURNs = append(depURNs, depURN)
				depset[depURN] = true
			}
		}
		pdeps[key] = deps
	}

	return pmap, pdeps, depURNs, nil
}

// `gosec` thinks these are credentials, but they are not.
// nolint: gosec
const rpcTokenUnknownValue = "04da6b54-80e4-46f7-96ec-b56ff0331ba9"

const cannotAwaitFmt = "cannot marshal Output value of type %T; please use Apply to access the Output's value"

// marshalInput marshals an input value, returning its raw serializable value along with any dependencies.
func marshalInput(v interface{}, await bool) (resource.PropertyValue, []Resource, error) {
	for {
		// If v is nil, just return that.
		if v == nil {
			return resource.PropertyValue{}, nil, nil
		}

		// If this is an Output, recurse.
		if out, ok := isOutput(v); ok {
			if !await {
				return resource.PropertyValue{}, nil, errors.Errorf(cannotAwaitFmt, v)
			}
			return marshalInputOutput(out)
		}

		// Next, look for some well known types.
		switch v := v.(type) {
		case *asset:
			return resource.NewAssetProperty(&resource.Asset{
				Path: v.Path(),
				Text: v.Text(),
				URI:  v.URI(),
			}), nil, nil
		case *archive:
			var assets map[string]interface{}
			if as := v.Assets(); as != nil {
				assets = make(map[string]interface{})
				for k, a := range as {
					aa, _, err := marshalInput(a, await)
					if err != nil {
						return resource.PropertyValue{}, nil, err
					}
					assets[k] = aa.V
				}
			}
			return resource.NewArchiveProperty(&resource.Archive{
				Assets: assets,
				Path:   v.Path(),
				URI:    v.URI(),
			}), nil, nil
		case anyInput:
			return marshalInput(v.v, await)
		case CustomResource:
			// Resources aren't serializable; instead, serialize a reference to ID, tracking as a dependency.
			e, d, err := marshalInput(v.GetID(), await)
			if err != nil {
				return resource.PropertyValue{}, nil, err
			}
			return e, append([]Resource{v}, d...), nil
		}

		rv := reflect.ValueOf(v)
		switch rv.Type().Kind() {
		case reflect.Bool:
			return resource.NewBoolProperty(rv.Bool()), nil, nil
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return resource.NewNumberProperty(float64(rv.Int())), nil, nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return resource.NewNumberProperty(float64(rv.Uint())), nil, nil
		case reflect.Float32, reflect.Float64:
			return resource.NewNumberProperty(rv.Float()), nil, nil
		case reflect.Ptr, reflect.Interface:
			// Dereference non-nil pointers and interfaces.
			if rv.IsNil() {
				return resource.PropertyValue{}, nil, nil
			}
			rv = rv.Elem()
		case reflect.String:
			return resource.NewStringProperty(rv.String()), nil, nil
		case reflect.Array, reflect.Slice:
			// If an array or a slice, create a new array by recursing into elements.
			var arr []resource.PropertyValue
			var deps []Resource
			for i := 0; i < rv.Len(); i++ {
				elem := rv.Index(i)
				e, d, err := marshalInput(elem.Interface(), await)
				if err != nil {
					return resource.PropertyValue{}, nil, err
				}
				arr = append(arr, e)
				deps = append(deps, d...)
			}
			return resource.NewArrayProperty(arr), deps, nil
		case reflect.Map:
			if rv.Type().Key().Kind() != reflect.String {
				return resource.PropertyValue{}, nil,
					errors.Errorf("expected map keys to be strings; got %v", rv.Type().Key())
			}

			// For maps, only support string-based keys, and recurse into the values.
			obj := resource.PropertyMap{}
			var deps []Resource
			for _, key := range rv.MapKeys() {
				value := rv.MapIndex(key)
				mv, d, err := marshalInput(value.Interface(), await)
				if err != nil {
					return resource.PropertyValue{}, nil, err
				}

				obj[resource.PropertyKey(key.String())] = mv
				deps = append(deps, d...)
			}
			return resource.NewObjectProperty(obj), deps, nil
		case reflect.Struct:
			obj := resource.PropertyMap{}
			typ := rv.Type()
			var deps []Resource
			for i := 0; i < typ.NumField(); i++ {
				tag := typ.Field(i).Tag.Get("pulumi")
				if tag == "" {
					continue
				}

				fv, d, err := marshalInput(rv.Field(i).Interface(), await)
				if err != nil {
					return resource.PropertyValue{}, nil, err
				}

				obj[resource.PropertyKey(tag)] = fv
				deps = append(deps, d...)
			}
			return resource.NewObjectProperty(obj), deps, nil
		default:
			return resource.PropertyValue{}, nil, errors.Errorf("unrecognized input property type: %v (%T)", v, v)
		}
		v = rv.Interface()
	}
}

func marshalInputOutput(out Output) (resource.PropertyValue, []Resource, error) {
	// Await the value and return its raw value.
	ov, known, err := out.await(context.TODO())
	if err != nil {
		return resource.PropertyValue{}, nil, err
	}

	// If the value is known, marshal it.
	if known {
		e, d, merr := marshalInput(ov, true)
		if merr != nil {
			return resource.PropertyValue{}, nil, merr
		}
		return e, append(out.dependencies(), d...), nil
	}

	// Otherwise, simply return the unknown value sentinel.
	return resource.MakeComputed(resource.NewStringProperty("")), out.dependencies(), nil
}

func unmarshalPropertyValue(v resource.PropertyValue) (interface{}, error) {
	switch {
	case v.IsComputed() || v.IsOutput():
		return nil, nil
	case v.IsSecret():
		return nil, errors.New("this version of the Pulumi SDK does not support first-class secrets")
	case v.IsArray():
		arr := v.ArrayValue()
		rv := make([]interface{}, len(arr))
		for i, e := range arr {
			ev, err := unmarshalPropertyValue(e)
			if err != nil {
				return nil, err
			}
			rv[i] = ev
		}
		return rv, nil
	case v.IsObject():
		m := make(map[string]interface{})
		for k, e := range v.ObjectValue() {
			ev, err := unmarshalPropertyValue(e)
			if err != nil {
				return nil, err
			}
			m[string(k)] = ev
		}
		return m, nil
	case v.IsAsset():
		asset := v.AssetValue()
		switch {
		case asset.IsPath():
			return NewFileAsset(asset.Path), nil
		case asset.IsText():
			return NewStringAsset(asset.Text), nil
		case asset.IsURI():
			return NewRemoteAsset(asset.URI), nil
		}
		return nil, errors.New("expected asset to be one of File, String, or Remote; got none")
	case v.IsArchive():
		archive := v.ArchiveValue()
		switch {
		case archive.IsAssets():
			as := make(map[string]interface{})
			for k, v := range archive.Assets {
				a, err := unmarshalPropertyValue(resource.NewPropertyValue(v))
				if err != nil {
					return nil, err
				}
				as[k] = a
			}
			return NewAssetArchive(as), nil
		case archive.IsPath():
			return NewFileArchive(archive.Path), nil
		case archive.IsURI():
			return NewRemoteArchive(archive.URI), nil
		default:
		}
		return nil, errors.New("expected asset to be one of File, String, or Remote; got none")
	default:
		return v.V, nil
	}
}

// unmarshalOutput unmarshals a single output variable into its runtime representation.
func unmarshalOutput(v resource.PropertyValue, dest reflect.Value) error {
	contract.Assert(dest.CanSet())

	// Check for nils and unknowns. The destination will be left with the zero value.
	if v.IsNull() || v.IsComputed() || v.IsOutput() {
		return nil
	}

	// Allocate storage as necessary.
	for dest.Kind() == reflect.Ptr {
		elem := reflect.New(dest.Type().Elem())
		dest.Set(elem)
		dest = elem.Elem()
	}

	// In the case of assets and archives, turn these into real asset and archive structures.
	switch {
	case v.IsAsset():
		if !assetType.Elem().AssignableTo(dest.Type()) {
			return errors.Errorf("expected a %s, got an asset", dest.Type())
		}

		asset, err := unmarshalPropertyValue(v)
		if err != nil {
			return err
		}
		dest.Set(reflect.ValueOf(asset).Elem())
		return nil
	case v.IsArchive():
		if !archiveType.Elem().AssignableTo(dest.Type()) {
			return errors.Errorf("expected a %s, got an archive", dest.Type())
		}

		archive, err := unmarshalPropertyValue(v)
		if err != nil {
			return err
		}
		dest.Set(reflect.ValueOf(archive).Elem())
		return nil
	case v.IsSecret():
		return errors.New("this version of the Pulumi SDK does not support first-class secrets")
	}

	// Unmarshal based on the desired type.
	switch dest.Kind() {
	case reflect.Bool:
		if !v.IsBool() {
			return errors.Errorf("expected a %v, got a %s", dest.Type(), v.TypeString())
		}
		dest.SetBool(v.BoolValue())
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if !v.IsNumber() {
			return errors.Errorf("expected an %v, got a %s", dest.Type(), v.TypeString())
		}
		dest.SetInt(int64(v.NumberValue()))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !v.IsNumber() {
			return errors.Errorf("expected an %v, got a %s", dest.Type(), v.TypeString())
		}
		dest.SetUint(uint64(v.NumberValue()))
		return nil
	case reflect.Float32, reflect.Float64:
		if !v.IsNumber() {
			return errors.Errorf("expected an %v, got a %s", dest.Type(), v.TypeString())
		}
		dest.SetFloat(v.NumberValue())
		return nil
	case reflect.String:
		if !v.IsString() {
			return errors.Errorf("expected a %v, got a %s", dest.Type(), v.TypeString())
		}
		dest.SetString(v.StringValue())
		return nil
	case reflect.Slice:
		if !v.IsArray() {
			return errors.Errorf("expected a %v, got a %s", dest.Type(), v.TypeString())
		}
		arr := v.ArrayValue()
		slice := reflect.MakeSlice(dest.Type(), len(arr), len(arr))
		for i, e := range arr {
			if err := unmarshalOutput(e, slice.Index(i)); err != nil {
				return err
			}
		}
		dest.Set(slice)
		return nil
	case reflect.Map:
		if !v.IsObject() {
			return errors.Errorf("expected a %v, got a %s", dest.Type(), v.TypeString())
		}

		keyType, elemType := dest.Type().Key(), dest.Type().Elem()
		if keyType.Kind() != reflect.String {
			return errors.Errorf("map keys must be assignable from type string")
		}

		result := reflect.MakeMap(dest.Type())
		for k, e := range v.ObjectValue() {
			elem := reflect.New(elemType).Elem()
			if err := unmarshalOutput(e, elem); err != nil {
				return err
			}

			key := reflect.New(keyType).Elem()
			key.SetString(string(k))

			result.SetMapIndex(key, elem)
		}
		dest.Set(result)
		return nil
	case reflect.Interface:
		if !anyType.Implements(dest.Type()) {
			return errors.Errorf("cannot unmarshal into non-empty interface type %v", dest.Type())
		}

		// If we're unmarshaling into the empty interface type, use the property type as the type of the result.
		result, err := unmarshalPropertyValue(v)
		if err != nil {
			return err
		}
		dest.Set(reflect.ValueOf(result))
		return nil
	case reflect.Struct:
		if !v.IsObject() {
			return errors.Errorf("expected a %v, got a %s", dest.Type(), v.TypeString())
		}

		obj := v.ObjectValue()
		typ := dest.Type()
		for i := 0; i < typ.NumField(); i++ {
			fieldV := dest.Field(i)
			if !fieldV.CanSet() {
				continue
			}

			tag := typ.Field(i).Tag.Get("pulumi")
			if tag == "" {
				continue
			}

			e, ok := obj[resource.PropertyKey(tag)]
			if !ok {
				continue
			}

			if err := unmarshalOutput(e, fieldV); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.Errorf("cannot unmarshal into type %v", dest.Type())
	}
}
