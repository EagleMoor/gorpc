package adapter

import (
	"bytes"
	"github.com/sergei-svistunov/gorpc"
	"go/format"
	"lazada_api/common/log"
	"net/http"
	"reflect"
	"regexp"
	"strings"
)

// define default values for params
var (
	pkgName      = "adapter"
	internalPkgs = []string{"lazada_api"}
	serviceName  = "ExternalAPI"
)

type AdapterHandler struct {
	hm   *gorpc.HandlersManager
	code []byte
}

func NewJSONClientLibGeneratorHandler(hm *gorpc.HandlersManager) *AdapterHandler {
	return &AdapterHandler{
		hm: hm,
	}
}

func (h *AdapterHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		w.Write([]byte(err.Error()))
	}
	if h := req.Form.Get("help"); h != "" {
		w.Write([]byte(usageInfo))
		return
	}
	if pkg := req.Form.Get("package"); pkg != "" {
		pkgName = pkg
	}
	if pkg, ok := req.Form["internal_pkg"]; ok {
		internalPkgs = pkg
	}
	if srvName := req.Form.Get("service_name"); srvName != "" {
		serviceName = srvName
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	code, err := generateAdapterCode(h.hm)
	if err != nil {
		w.Write([]byte(err.Error()))
		return
	}
	w.Write(code)
}

var usageInfo = `
Possible get params:
    package - package name for generated code ('` + pkgName + `' is default)
    internal_pkg - package names prefix that will be copied into generated code to avoid big imports
                   For example: 'internal_pkg=lazada_api&internal_pkg=mobapi'
    service_name - name of service ('` + serviceName + `' is default)
`

type handlerInfo struct {
	Params []gorpc.HandlerParameter `json:"params"`
	Output string                   `json:"output"`
	Input  string                   `json:"input"`
}

var path2HandlerInfoMapping = map[string]handlerInfo{}
var existStructsStack = ExistStructs{}

func generateAdapterCode(hm *gorpc.HandlersManager) ([]byte, error) {
	structsBuf := &bytes.Buffer{}
	extraImports := []string{}

	if err := collectStructs(hm, structsBuf, &extraImports); err != nil {
		return nil, err
	}

	result := regexp.MustCompilePOSIX(">>>PKG_NAME<<<").ReplaceAll(mainTemplate, []byte(pkgName))

	if staticComp := getComponentByPlaceholder(">>>STATIC_LOGIC<<<"); staticComp != nil {
		result = regexp.MustCompilePOSIX(">>>STATIC_LOGIC<<<").ReplaceAll(result, staticComp.GetCode())
	}

	if caller := getComponentByPlaceholder(">>>CALLER<<<"); caller != nil {
		result = regexp.MustCompilePOSIX(">>>CALLER<<<").ReplaceAll(result, caller.GetCode())
	}

	if error := getComponentByPlaceholder(">>>ERRORS<<<"); error != nil {
		result = regexp.MustCompilePOSIX(">>>ERRORS<<<").ReplaceAll(result, error.GetCode())
	}

	result = regexp.MustCompilePOSIX(">>>DYNAMIC_LOGIC<<<").ReplaceAll(result, generateAdapterMethods(structsBuf))
	result = regexp.MustCompilePOSIX(">>>STRUCTS<<<").ReplaceAll(result, structsBuf.Bytes())
	result = regexp.MustCompilePOSIX(">>>IMPORTS<<<").ReplaceAll(result, []byte(CollectImports(extraImports)))

	return format.Source(result)
}

var mainTemplate = []byte(`// It's autogenerated file. It's not recommended to modify it.
package >>>PKG_NAME<<<

import (
>>>IMPORTS<<<
)

>>>DYNAMIC_LOGIC<<<

>>>STATIC_LOGIC<<<

>>>STRUCTS<<<

`)

func collectStructs(hm *gorpc.HandlersManager, structsBuf *bytes.Buffer, extraImports *[]string) error {
	for _, path := range hm.GetHandlersPaths() {
		info := hm.GetHandlerInfo(path)
		for _, v := range info.Versions {
			handlerOutputTypeName, err := convertStructToCode(v.GetMethod().Type.Out(0), structsBuf, extraImports)
			if err != nil {
				return err
			}
			handlerIntputTypeName, err := convertStructToCode(v.GetMethod().Type.In(2), structsBuf, extraImports)
			if err != nil {
				return err
			}
			path2HandlerInfoMapping[path+"/"+v.GetVersion()] = handlerInfo{
				Params: v.Request.Fields,
				Output: handlerOutputTypeName,
				Input:  handlerIntputTypeName,
			}
		}
	}
	return nil
}

func convertStructToCode(t reflect.Type, codeBuf *bytes.Buffer, extraImports *[]string) (typeName string, err error) {
	// ignore slice of new types because this type exactly new and we're collecting its content right now below
	typeName, _ = detectTypeName(t, extraImports)
	if strings.Contains(typeName, ".") {
		// do not migrate external structs (type name with path)
		return
	}

	var newInternalTypes []reflect.Type

	defer func() {
		for _, newType := range newInternalTypes {
			if _, err = convertStructToCode(newType, codeBuf, extraImports); err != nil {
				return
			}
		}
	}()

	switch t.Kind() {
	case reflect.Struct:
		str := "type " + typeName + " struct {\n"
		for i := 0; i < t.NumField(); i++ {

			field := t.Field(i)

			fieldName, emb := detectTypeName(field.Type, extraImports)
			if emb != nil {
				newInternalTypes = append(newInternalTypes, emb...)
			}

			if field.Anonymous {
				str += ("	" + fieldName)
			} else {
				str += ("	" + field.Name + " " + fieldName)
				if jsonTag := field.Tag.Get("json"); jsonTag != "" {
					str += (" `json:\"" + jsonTag + "\"`")
				}
			}

			str += "\n"
		}
		str += "}\n\n"

		codeBuf.WriteString(str)

		return
	case reflect.Ptr:
		return convertStructToCode(t.Elem(), codeBuf, extraImports)
	case reflect.Slice:
		var elemType string
		elemType, err = convertStructToCode(t.Elem(), codeBuf, extraImports)
		if err != nil {
			return
		}
		sliceType := "[]" + elemType
		if typeName != sliceType {
			writeType(codeBuf, typeName, sliceType)
		}

		return
	case reflect.Map:
		keyType, _ := convertStructToCode(t.Key(), codeBuf, extraImports)
		valType, _ := convertStructToCode(t.Elem(), codeBuf, extraImports)

		if mapName := "map[" + keyType + "]" + valType; typeName != mapName {
			writeType(codeBuf, typeName, mapName)
		}
		return
	default:
		// if type is custom we need to describe it in code
		if typeName != t.Kind().String() && typeName != "interface{}" {
			writeType(codeBuf, typeName, t.Kind().String())
			return
		}
	}

	return
}

func writeType(codeBuf *bytes.Buffer, name, kind string) {
	codeBuf.WriteString("type " + name + " " + kind + "\n\n")
}

func isInternalType(pkgPath string) bool {
	for _, pkgName := range internalPkgs {
		if strings.HasPrefix(pkgPath, pkgName) {
			return true
		}
	}
	return false
}

func detectTypeName(t reflect.Type, extraImports *[]string) (name string, newTypes []reflect.Type) {
	name = t.Name()
	if name != "" {
		// for custom types make unique names using package path
		// because different packages can contains structs with same names
		if isInternalType(t.PkgPath()) {
			path := strings.Replace(t.PkgPath(), "/", "_", -1)
			path = strings.Title(path)

			name = path + "_" + strings.Title(name)

			if !existStructsStack.AlreadyExist(name) {
				newTypes = []reflect.Type{t}
				existStructsStack.Add(name)
			}
		} else if t.PkgPath() != "" {
			//log.Debugf("Append extra import: %s", t.PkgPath())
			*extraImports = append(*extraImports, t.PkgPath())
			name = t.String()
		}

		name = strings.Replace(name, "-", "_", -1)

		return name, newTypes
	}

	// some types has no name so we need to make it manually
	name = "interface{}"
	switch t.Kind() {
	case reflect.Slice:
		name, embeded := detectTypeName(t.Elem(), extraImports)
		if embeded != nil {
			newTypes = append(newTypes, embeded...)
		}
		return "[]" + name, newTypes
	case reflect.Map:
		// TODO enhance for custom key type in map
		key := t.Key().Name()
		val, embeded := detectTypeName(t.Elem(), extraImports)
		if embeded != nil {
			newTypes = append(newTypes, embeded...)
		}
		if isInternalType(t.Elem().PkgPath()) {
			if !existStructsStack.AlreadyExist(name) {
				newTypes = []reflect.Type{t}
				existStructsStack.Add(name)
			}
		}
		if key != "" && val != "" {
			return "map[" + key + "]" + val, newTypes
		}
	case reflect.Ptr:
		name, embeded := detectTypeName(t.Elem(), extraImports)
		if embeded != nil {
			newTypes = append(newTypes, embeded...)
		}
		return name, newTypes
	case reflect.Interface:
		return
	}

	log.Error("Unknown type has been replaced with interface{}")
	return
}
