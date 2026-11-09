package apiclient

import (
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// Query encodes one of the contract's query structs (api.JobQuery, ...) by its json
// tags, so query parameter names cannot drift from the contract (I171). Zero values
// are omitted; embedded structs are flattened.
func Query(q any) url.Values {
	out := url.Values{}
	if q == nil {
		return out
	}
	encodeStruct(out, reflect.ValueOf(q))
	return out
}

func encodeStruct(out url.Values, v reflect.Value) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous {
			encodeStruct(out, v.Field(i))
			continue
		}
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fv := v.Field(i)
		if fv.IsZero() {
			continue
		}
		switch fv.Kind() {
		case reflect.String:
			out.Set(name, fv.String())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			out.Set(name, strconv.FormatInt(fv.Int(), 10))
		case reflect.Bool:
			out.Set(name, strconv.FormatBool(fv.Bool()))
		}
	}
}
