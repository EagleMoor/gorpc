package adapter

import (
	"bytes"
	"go/format"
	"log"
	"reflect"
	"regexp"
	"strings"

	"fmt"
	"github.com/sergei-svistunov/gorpc"
)

type handlerInfo struct {
	Output string
	Input  string
	Errors []gorpc.HandlerError
}

type HttpJsonLibGenerator struct {
	hm                      *gorpc.HandlersManager
	pkgName                 string
	serviceName             string
	path2HandlerInfoMapping map[string]handlerInfo
	collectedStructs        map[string]struct{}
	extraImports            map[string]struct{}
	convertedStructs        map[reflect.Type]string
}

func NewHttpJsonLibGenerator(hm *gorpc.HandlersManager, packageName, serviceName string) *HttpJsonLibGenerator {
	generator := HttpJsonLibGenerator{
		hm:                      hm,
		pkgName:                 "adapter",
		serviceName:             "ExternalAPI",
		path2HandlerInfoMapping: map[string]handlerInfo{},
		collectedStructs:        map[string]struct{}{},
		extraImports:            map[string]struct{}{},
		convertedStructs:        map[reflect.Type]string{},
	}
	if packageName != "" {
		generator.pkgName = packageName
	}
	if serviceName != "" {
		generator.serviceName = serviceName
	}

	return &generator
}

func (g *HttpJsonLibGenerator) Generate() ([]byte, error) {
	clientStructs, err := g.collectStructs()
	if err != nil {
		return nil, err
	}

	result := regexp.MustCompilePOSIX(">>>API_NAME<<<").ReplaceAll(mainTemplate, []byte(g.getAPIName()))
	result = regexp.MustCompilePOSIX(">>>PKG_NAME<<<").ReplaceAll(result, []byte(g.pkgName))
	result = regexp.MustCompilePOSIX(">>>CLIENT_API_FUNCS<<<").ReplaceAll(result, g.generateAdapterMethods())
	result = regexp.MustCompilePOSIX(">>>CLIENT_STRUCTS<<<").ReplaceAll(result, clientStructs)
	result = regexp.MustCompilePOSIX(">>>IMPORTS<<<").ReplaceAll(result, g.collectImports())

	return format.Source(result)
}

func (g *HttpJsonLibGenerator) getAPIName() string {
	return strings.Title(g.serviceName)
}

func (g *HttpJsonLibGenerator) collectStructs() ([]byte, error) {
	structsBuf := &bytes.Buffer{}
	for _, path := range g.hm.GetHandlersPaths() {
		info := g.hm.GetHandlerInfo(path)
		for _, v := range info.Versions {
			handlerOutputTypeName, err := g.convertStructToCode(v.Response, structsBuf)
			if err != nil {
				return nil, err
			}
			handlerInputTypeName, err := g.convertStructToCode(v.Request.Type, structsBuf)
			if err != nil {
				return nil, err
			}
			g.path2HandlerInfoMapping[v.Route] = handlerInfo{
				Output: handlerOutputTypeName,
				Input:  handlerInputTypeName,
				Errors: v.Errors,
			}
		}
	}
	return structsBuf.Bytes(), nil
}

func (g *HttpJsonLibGenerator) generateAdapterMethods() []byte {
	var result bytes.Buffer
	var handlerErrorsBuf bytes.Buffer

	for path, handlerInfo := range g.path2HandlerInfoMapping {
		name := strings.Replace(strings.Title(path), "/", "", -1)
		name = strings.Replace(name, "_", "", -1)

		handlerErrorsNameMapping := "nil"
		if len(handlerInfo.Errors) > 0 {
			handlerErrorsName := name + "Errors"
			fmt.Fprintf(&handlerErrorsBuf, "type %s int\n\n", handlerErrorsName)
			handlerErrorsBuf.WriteString("const (\n")
			for i, e := range handlerInfo.Errors {
				if i == 0 {
					fmt.Fprintf(&handlerErrorsBuf, "%s_%s = iota\n", handlerErrorsName, e.Code)
				} else {
					fmt.Fprintf(&handlerErrorsBuf, "%s_%s\n", handlerErrorsName, e.Code)
				}
			}
			handlerErrorsBuf.WriteString(")\n\n")
			fmt.Fprintf(&handlerErrorsBuf, "var _%sMapping = map[string]int{\n", handlerErrorsName)
			for _, e := range handlerInfo.Errors {
				fmt.Fprintf(&handlerErrorsBuf, "\"%s\": %s_%s,\n", e.Code, handlerErrorsName, e.Code)
			}
			handlerErrorsBuf.WriteString("}\n\n")
			handlerErrorsNameMapping = "_" + handlerErrorsName + "Mapping"
		}

		method := regexp.MustCompilePOSIX(">>>HANDLER_PATH<<<").ReplaceAll(handlerCallPostFuncTemplate, []byte(path))
		method = regexp.MustCompilePOSIX(">>>HANDLER_NAME<<<").ReplaceAll(method, []byte(name))
		method = regexp.MustCompilePOSIX(">>>INPUT_TYPE<<<").ReplaceAll(method, []byte(handlerInfo.Input))
		method = regexp.MustCompilePOSIX(">>>RETURNED_TYPE<<<").ReplaceAll(method, []byte(handlerInfo.Output))
		method = regexp.MustCompilePOSIX(">>>HANDLER_ERRORS<<<").ReplaceAll(method, []byte(handlerErrorsNameMapping))
		method = regexp.MustCompilePOSIX(">>>API_NAME<<<").ReplaceAll(method, []byte(g.getAPIName()))

		result.Write(method)
	}

	handlerErrorsBuf.WriteTo(&result)

	return result.Bytes()
}

func (g *HttpJsonLibGenerator) needToMigratePkgStructs(pkgPath string) bool {
	// TODO this check was removed and all types with non-empty package path will be migrated in library code
	return pkgPath != ""
}

func (g *HttpJsonLibGenerator) convertStructToCode(t reflect.Type, codeBuf *bytes.Buffer) (typeName string, err error) {
	if name, ok := g.convertedStructs[t]; ok {
		return name, nil
	}
	defer func() {
		g.convertedStructs[t] = typeName
	}()

	// ignore slice of new types because this type exactly new and we're collecting its content right now below
	typeName, _ = g.detectTypeName(t)
	if strings.Contains(typeName, ".") {
		// do not migrate external structs (type name with path)
		return
	}

	var newInternalTypes []reflect.Type

	defer func() {
		for _, newType := range newInternalTypes {
			if _, err = g.convertStructToCode(newType, codeBuf); err != nil {
				return
			}
		}
	}()

	switch t.Kind() {
	case reflect.Struct:
		str := "type " + typeName + " struct {\n"
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			fieldName, emb := g.detectTypeName(field.Type)
			if emb != nil {
				newInternalTypes = append(newInternalTypes, emb...)
			}

			if field.Anonymous {
				str += ("	" + fieldName)
			} else {
				str += ("	" + field.Name + " " + fieldName)
				jsonTag := field.Tag.Get("json")
				if jsonTag == "" {
					jsonTag = field.Tag.Get("key")
				}
				if jsonTag != "" {
					str += (" `json:\"" + jsonTag + "\"`")
				}
			}

			str += "\n"
		}
		str += "}\n\n"

		codeBuf.WriteString(str)

		return
	case reflect.Ptr:
		return g.convertStructToCode(t.Elem(), codeBuf)
	case reflect.Slice:
		var elemType string
		elemType, err = g.convertStructToCode(t.Elem(), codeBuf)
		if err != nil {
			return
		}
		sliceType := "[]" + elemType
		if typeName != sliceType {
			writeType(codeBuf, typeName, sliceType)
		}

		return
	case reflect.Map:
		keyType, _ := g.convertStructToCode(t.Key(), codeBuf)
		valType, _ := g.convertStructToCode(t.Elem(), codeBuf)

		mapName := "map[" + keyType + "]" + valType
		if typeName != mapName {
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
	fmt.Fprintf(codeBuf, "type %s %s\n\n", name, kind)
}

func (g *HttpJsonLibGenerator) migratedStructName(t reflect.Type) string {
	path := t.PkgPath()
	if strings.HasPrefix(path, g.hm.Pkg()) {
		path = strings.TrimPrefix(path, g.hm.Pkg())
	}
	if strings.HasPrefix(path, "/") {
		path = strings.TrimPrefix(path, "/")
	}
	path = strings.Replace(path, "/", " ", -1)
	path = strings.Replace(path, ".", "", -1)
	name := strings.Title(path + " " + t.Name())
	name = strings.Replace(name, " ", "", -1)
	return name
}

func (g *HttpJsonLibGenerator) detectTypeName(t reflect.Type) (name string, newTypes []reflect.Type) {
	name = t.Name()
	if name != "" {
		// for custom types make unique names using package path
		// because different packages can contains structs with same names
		if g.needToMigratePkgStructs(t.PkgPath()) {
			name = g.migratedStructName(t)

			if _, exists := g.collectedStructs[name]; !exists {
				newTypes = []reflect.Type{t}
				g.collectedStructs[name] = struct{}{}
			}
		} else if t.PkgPath() != "" {
			g.extraImports[t.PkgPath()] = struct{}{}
			name = t.String()
		}

		name = strings.Replace(name, "-", "_", -1)

		return name, newTypes
	}

	// some types has no name so we need to make it manually
	name = "interface{}"
	switch t.Kind() {
	case reflect.Slice:
		name, embedded := g.detectTypeName(t.Elem())
		if embedded != nil {
			newTypes = append(newTypes, embedded...)
		}
		return "[]" + name, newTypes
	case reflect.Map:
		// TODO enhance for custom key type in map
		key := t.Key().Name()
		val, embedded := g.detectTypeName(t.Elem())
		if embedded != nil {
			newTypes = append(newTypes, embedded...)
		}
		if g.needToMigratePkgStructs(t.Elem().PkgPath()) {
			if _, exists := g.collectedStructs[name]; !exists {
				newTypes = []reflect.Type{t}
				g.collectedStructs[name] = struct{}{}
			}
		}
		if key != "" && val != "" {
			return "map[" + key + "]" + val, newTypes
		}
	case reflect.Ptr:
		name, embedded := g.detectTypeName(t.Elem())
		if embedded != nil {
			newTypes = append(newTypes, embedded...)
		}
		return name, newTypes
	case reflect.Interface:
		return
	default:
		log.Println("Unknown type has been replaced with interface{}")
		return
	}

	return
}

func (g *HttpJsonLibGenerator) collectImports() []byte {
	var buf bytes.Buffer
	for _, _import := range mainImports {
		appendImport(&buf, _import)
	}
	for _import, _ := range g.extraImports {
		appendImport(&buf, _import)
	}
	return buf.Bytes()
}

func appendImport(buf *bytes.Buffer, _import string) {
	if strings.HasSuffix(_import, `"`) {
		fmt.Fprintln(buf, _import)
	} else {
		fmt.Fprintf(buf, "\"%s\"\n", _import)
	}
}
