// Package reflectx implements extensions to the standard reflect lib suitable
// for implementing marshalling and unmarshalling packages.  The main Mapper type
// allows for Go-compatible named attribute access, including accessing embedded
// struct attributes and the ability to use  functions and struct tags to
// customize field names.
//
package reflectx

import (
	"reflect"
	"runtime"
	"strings"
	"sync"
)

// A FieldInfo is metadata for a struct field.
type FieldInfo struct {
	Index    []int  // 之所以index是一个数组，是因为有层级关系
	Path     string // path中含层级关系，比如person.first_name
	Field    reflect.StructField
	Zero     reflect.Value
	Name     string            // first_name
	Options  map[string]string // json:"xxx,omitempty" 处理其中的omitempty
	Embedded bool
	Children []*FieldInfo // 维护树形关系
	Parent   *FieldInfo
}

// A StructMap is an index of field metadata for a struct.
type StructMap struct {
	Tree  *FieldInfo
	Index []*FieldInfo
	Paths map[string]*FieldInfo // 看起来用的不多
	Names map[string]*FieldInfo // 比起paths更常用
}

// GetByPath returns a *FieldInfo for a given string path.
func (f StructMap) GetByPath(path string) *FieldInfo {
	return f.Paths[path]
}

// GetByTraversal returns a *FieldInfo for a given integer path.  It is
// analogous to reflect.FieldByIndex, but using the cached traversal
// rather than re-executing the reflect machinery each time.
func (f StructMap) GetByTraversal(index []int) *FieldInfo {
	if len(index) == 0 {
		return nil
	}

	tree := f.Tree
	for _, i := range index {
		if i >= len(tree.Children) || tree.Children[i] == nil {
			return nil
		}
		tree = tree.Children[i]
	}
	return tree
}

// Mapper is a general purpose mapper of names to struct fields.  A Mapper
// behaves like most marshallers in the standard library, obeying a field tag
// for name mapping but also providing a basic transform function.
type Mapper struct {
	cache      map[reflect.Type]*StructMap // 缓存所有类型的structMap
	tagName    string                      // struct的tag名称
	tagMapFunc func(string) string
	mapFunc    func(string) string
	mutex      sync.Mutex
}

// NewMapper returns a new mapper using the tagName as its struct field tag.
// If tagName is the empty string, it is ignored.
func NewMapper(tagName string) *Mapper {
	return &Mapper{
		cache:   make(map[reflect.Type]*StructMap),
		tagName: tagName,
	}
}

// NewMapperTagFunc returns a new mapper which contains a mapper for field names
// AND a mapper for tag values.  This is useful for tags like json which can
// have values like "name,omitempty".
func NewMapperTagFunc(tagName string, mapFunc, tagMapFunc func(string) string) *Mapper {
	return &Mapper{
		cache:      make(map[reflect.Type]*StructMap),
		tagName:    tagName,
		mapFunc:    mapFunc,
		tagMapFunc: tagMapFunc,
	}
}

// NewMapperFunc returns a new mapper which optionally obeys a field tag and
// a struct field name mapper func given by f.  Tags will take precedence, but
// for any other field, the mapped name will be f(field.Name)
func NewMapperFunc(tagName string, f func(string) string) *Mapper {
	return &Mapper{
		cache:   make(map[reflect.Type]*StructMap),
		tagName: tagName,
		mapFunc: f,
	}
}

// TypeMap returns a mapping of field strings to int slices representing
// the traversal down the struct to reach the field.
func (m *Mapper) TypeMap(t reflect.Type) *StructMap {
	m.mutex.Lock()
	mapping, ok := m.cache[t]
	if !ok {
		mapping = getMapping(t, m.tagName, m.mapFunc, m.tagMapFunc)
		m.cache[t] = mapping
	}
	m.mutex.Unlock()
	return mapping
}

// FieldMap returns the mapper's mapping of field names to reflect values.  Panics
// if v's Kind is not Struct, or v is not Indirectable to a struct kind.
func (m *Mapper) FieldMap(v reflect.Value) map[string]reflect.Value {
	v = reflect.Indirect(v)
	mustBe(v, reflect.Struct)

	r := map[string]reflect.Value{}
	tm := m.TypeMap(v.Type())
	for tagName, fi := range tm.Names {
		r[tagName] = FieldByIndexes(v, fi.Index)
	}
	return r
}

// FieldByName returns a field by its mapped name as a reflect.Value.
// Panics if v's Kind is not Struct or v is not Indirectable to a struct Kind.
// Returns zero Value if the name is not found.
func (m *Mapper) FieldByName(v reflect.Value, name string) reflect.Value {
	v = reflect.Indirect(v)
	mustBe(v, reflect.Struct)

	tm := m.TypeMap(v.Type())
	fi, ok := tm.Names[name]
	if !ok {
		return v
	}
	return FieldByIndexes(v, fi.Index)
}

// FieldsByName returns a slice of values corresponding to the slice of names
// for the value.  Panics if v's Kind is not Struct or v is not Indirectable
// to a struct Kind.  Returns zero Value for each name not found.
func (m *Mapper) FieldsByName(v reflect.Value, names []string) []reflect.Value {
	v = reflect.Indirect(v)
	mustBe(v, reflect.Struct)

	tm := m.TypeMap(v.Type())
	vals := make([]reflect.Value, 0, len(names))
	for _, name := range names {
		fi, ok := tm.Names[name]
		if !ok {
			vals = append(vals, *new(reflect.Value))
		} else {
			vals = append(vals, FieldByIndexes(v, fi.Index))
		}
	}
	return vals
}

// TraversalsByName returns a slice of int slices which represent the struct
// traversals for each mapped name.  Panics if t is not a struct or Indirectable
// to a struct.  Returns empty int slice for each name not found.
func (m *Mapper) TraversalsByName(t reflect.Type, names []string) [][]int {
	r := make([][]int, 0, len(names))
	m.TraversalsByNameFunc(t, names, func(_ int, i []int) error {
		if i == nil {
			r = append(r, []int{})
		} else {
			r = append(r, i)
		}

		return nil
	})
	return r
}

// TraversalsByNameFunc traverses the mapped names and calls fn with the index of
// each name and the struct traversal represented by that name. Panics if t is not
// a struct or Indirectable to a struct. Returns the first error returned by fn or nil.
func (m *Mapper) TraversalsByNameFunc(t reflect.Type, names []string, fn func(int, []int) error) error {
	t = Deref(t)
	mustBe(t, reflect.Struct)
	// typeMap structMap，Mapper中有该类型的typeMap的缓存，一次进程启动，一个mapper是合理的
	tm := m.TypeMap(t)
	// 检索符合数据库返回column的fieldInfo.Index
	for i, name := range names {
		fi, ok := tm.Names[name]
		if !ok {
			if err := fn(i, nil); err != nil {
				return err
			}
		} else {
			if err := fn(i, fi.Index); err != nil {
				return err
			}
		}
	}
	return nil
}

// FieldByIndexes returns a value for the field given by the struct traversal
// for the given value.
func FieldByIndexes(v reflect.Value, indexes []int) reflect.Value {
	// 一层一层取field，根据indexes
	for _, i := range indexes {
		v = reflect.Indirect(v).Field(i)
		// if this is a pointer and it's nil, allocate a new value and set it
		if v.Kind() == reflect.Ptr && v.IsNil() {
			alloc := reflect.New(Deref(v.Type()))
			v.Set(alloc)
		}
		if v.Kind() == reflect.Map && v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
	}
	return v
}

// FieldByIndexesReadOnly returns a value for a particular struct traversal,
// but is not concerned with allocating nil pointers because the value is
// going to be used for reading and not setting.
func FieldByIndexesReadOnly(v reflect.Value, indexes []int) reflect.Value {
	for _, i := range indexes {
		v = reflect.Indirect(v).Field(i)
	}
	return v
}

// Deref is Indirect for reflect.Types
func Deref(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// -- helpers & utilities --

type kinder interface {
	Kind() reflect.Kind
}

// mustBe checks a value against a kind, panicing with a reflect.ValueError
// if the kind isn't that which is required.
func mustBe(v kinder, expected reflect.Kind) {
	if k := v.Kind(); k != expected {
		panic(&reflect.ValueError{Method: methodName(), Kind: k})
	}
}

// methodName returns the caller of the function calling methodName
func methodName() string {
	pc, _, _, _ := runtime.Caller(2)
	f := runtime.FuncForPC(pc)
	if f == nil {
		return "unknown method"
	}
	return f.Name()
}

type typeQueue struct {
	t  reflect.Type
	fi *FieldInfo
	pp string // Parent path
}

// A copying append that creates a new slice each time.
func apnd(is []int, i int) []int {
	x := make([]int, len(is)+1)
	copy(x, is)
	x[len(x)-1] = i
	return x
}

type mapf func(string) string

// parseName parses the tag and the target name for the given field using
// the tagName (eg 'json' for `json:"foo"` tags), mapFunc for mapping the
// field's name to a target name, and tagMapFunc for mapping the tag to
// a target name.
func parseName(field reflect.StructField, tagName string, mapFunc, tagMapFunc mapf) (tag, fieldName string) {
	// first, set the fieldName to the field's name
	fieldName = field.Name
	// if a mapFunc is set, use that to override the fieldName
	if mapFunc != nil {
		fieldName = mapFunc(fieldName)
	}

	// if there's no tag to look for, return the field name
	if tagName == "" {
		return "", fieldName
	}

	// if this tag is not set using the normal convention in the tag,
	// then return the fieldname..  this check is done because according
	// to the reflect documentation:
	//    If the tag does not have the conventional format,
	//    the value returned by Get is unspecified.
	// which doesn't sound great.
	if !strings.Contains(string(field.Tag), tagName+":") {
		return "", fieldName
	}

	// at this point we're fairly sure that we have a tag, so lets pull it out
	tag = field.Tag.Get(tagName)

	// if we have a mapper function, call it on the whole tag
	// XXX: this is a change from the old version, which pulled out the name
	// before the tagMapFunc could be run, but I think this is the right way
	if tagMapFunc != nil {
		tag = tagMapFunc(tag)
	}

	// finally, split the options from the name
	parts := strings.Split(tag, ",")
	fieldName = parts[0]

	return tag, fieldName
}

// parseOptions parses options out of a tag string, skipping the name
func parseOptions(tag string) map[string]string {
	parts := strings.Split(tag, ",")
	options := make(map[string]string, len(parts))
	if len(parts) > 1 {
		for _, opt := range parts[1:] {
			// short circuit potentially expensive split op
			if strings.Contains(opt, "=") {
				kv := strings.Split(opt, "=")
				options[kv[0]] = kv[1]
				continue
			}
			options[opt] = ""
		}
	}
	return options
}

// getMapping returns a mapping for the t type, using the tagName, mapFunc and
// tagMapFunc to determine the canonical names of fields.
func getMapping(t reflect.Type, tagName string, mapFunc, tagMapFunc mapf) *StructMap {
	// 从reflect.Type中反射出Field信息
	// 因为可能的struct是一个树形组织，需要使用DFS/BFS等算法遍历，会用到队列来解决递归问题的
	// 对应StructMap.Index数组
	m := []*FieldInfo{}

	root := &FieldInfo{}
	// 使用slice模拟queue
	queue := []typeQueue{}
	// type本身也算是一个fieldInfo，只是空的，是这么设定的
	queue = append(queue, typeQueue{Deref(t), root, ""})

QueueLoop:
	for len(queue) != 0 {
		// 取出queue头
		// pop the first item off of the queue
		tq := queue[0]
		queue = queue[1:]

		// 递归字段，类型里使用了自己这个类型，数据库model一般不会这么设计的
		// ignore recursive field
		for p := tq.fi.Parent; p != nil; p = p.Parent {
			if tq.fi.Field.Type == p.Field.Type {
				continue QueueLoop
			}
		}

		// children是当前类型的field数
		nChildren := 0
		if tq.t.Kind() == reflect.Struct {
			nChildren = tq.t.NumField()
		}
		tq.fi.Children = make([]*FieldInfo, nChildren)

		// iterate through all of its fields
		for fieldPos := 0; fieldPos < nChildren; fieldPos++ {

			// type.Field 类型
			f := tq.t.Field(fieldPos)

			// tag应该指的是`json:"xxx"`, name是xxx，是其中的内容
			// tagName一般是db
			// parse the tag and the target name using the mapping options for this field
			tag, name := parseName(f, tagName, mapFunc, tagMapFunc)

			// if the name is "-", disabled via a tag, skip it
			if name == "-" {
				continue
			}

			fi := FieldInfo{
				Field: f,
				Name:  name,
				Zero:  reflect.New(f.Type).Elem(),
				// 类比，json:"xxx,omitempty" 处理的是emitempty
				Options: parseOptions(tag),
			}

			// 有层级关系，使用path来存储
			// if the path is empty this path is just the name
			if tq.pp == "" {
				fi.Path = fi.Name
			} else {
				fi.Path = tq.pp + "." + fi.Name
			}

			// skip unexported fields
			if len(f.PkgPath) != 0 && !f.Anonymous {
				continue
			}

			// bfs search of anonymous embedded structs
			if f.Anonymous {
				pp := tq.pp
				if tag != "" {
					pp = fi.Path
				}

				fi.Embedded = true
				fi.Index = apnd(tq.fi.Index, fieldPos)
				nChildren := 0
				ft := Deref(f.Type)
				if ft.Kind() == reflect.Struct {
					nChildren = ft.NumField()
				}
				fi.Children = make([]*FieldInfo, nChildren)
				queue = append(queue, typeQueue{Deref(f.Type), &fi, pp})
			} else if fi.Zero.Kind() == reflect.Struct || (fi.Zero.Kind() == reflect.Ptr && fi.Zero.Type().Elem().Kind() == reflect.Struct) {
				// fieldInfo的Index，也是包含了层级关系，先是parent的fieldInfo.Index，然后补上自己的fieldPos
				fi.Index = apnd(tq.fi.Index, fieldPos)
				// TODO: 这里make了，但是具体内容没有填充啊？感觉是重复了。
				fi.Children = make([]*FieldInfo, Deref(f.Type).NumField())
				// bfs 广度优先，优先儿子，孙子靠后，通过queue来控制
				queue = append(queue, typeQueue{Deref(f.Type), &fi, fi.Path})
			}

			// 这里跟上面else if 重复了，覆盖
			fi.Index = apnd(tq.fi.Index, fieldPos)
			// 指定parent
			fi.Parent = tq.fi
			// 指定parent的children
			tq.fi.Children[fieldPos] = &fi
			m = append(m, &fi)
		}
	}

	// 把层级的field，拍扁平了
	flds := &StructMap{Index: m, Tree: root, Paths: map[string]*FieldInfo{}, Names: map[string]*FieldInfo{}}
	for _, fi := range flds.Index {
		// 根据path查找的map
		flds.Paths[fi.Path] = fi
		// Name是""的情况是什么，embedded是匿名的struct
		if fi.Name != "" && !fi.Embedded {
			flds.Names[fi.Path] = fi
		}
	}

	return flds
}
