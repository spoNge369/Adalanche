package engine

import (
	"encoding/xml"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OneOfOne/xxhash"
	"github.com/gofrs/uuid"
	"github.com/icza/gox/stringsx"
	jsoniter "github.com/json-iterator/go"
	"github.com/lkarlslund/adalanche/modules/dedup"
	"github.com/lkarlslund/adalanche/modules/ui"
	"github.com/lkarlslund/adalanche/modules/windowssecurity"
	"github.com/lkarlslund/stringdedup"
)

var adjustthreadsafe sync.Mutex
var threadsafeobject int
var threadbuckets = runtime.NumCPU() * 64

var threadsafeobjectmutexes = make([]sync.RWMutex, threadbuckets)

func init() {
	stringdedup.YesIKnowThisCouldGoHorriblyWrong = true
}

func setThreadsafe(enable bool) {
	adjustthreadsafe.Lock()
	if enable {
		threadsafeobject++
	} else {
		threadsafeobject--
	}
	if threadsafeobject < 0 {
		panic("threadsafeobject is negative")
	}
	adjustthreadsafe.Unlock()
}

var UnknownGUID = uuid.UUID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

type Object struct {
	values   AttributeValueMap
	edges    [2]EdgeConnections
	parent   *Object
	sdcache  *SecurityDescriptor
	sid      windowssecurity.SID
	children []*Object

	id   uint32
	guid uuid.UUID
	// objectcategoryguid uuid.UUID
	guidcached  bool
	sidcached   bool
	invalidated bool
	objecttype  ObjectType
}

var IgnoreBlanks = "_IGNOREBLANKS_"

func NewObject(flexinit ...interface{}) *Object {
	var result Object
	result.init()
	result.setFlex(flexinit...)

	return &result
}

func (o *Object) ID() uint32 {
	if o.id == 0 {
		panic("no ID set on object, where did it come from?")
	}
	return o.id
}

func (o *Object) lockbucket() int {
	return int(o.ID()) % threadbuckets
}

func (o *Object) lock() {
	if threadsafeobject != 0 {
		threadsafeobjectmutexes[o.lockbucket()].Lock()
	}
	if o.invalidated {
		panic("object is invalidated")
	}
}

func (o *Object) rlock() {
	if threadsafeobject != 0 {
		threadsafeobjectmutexes[o.lockbucket()].RLock()
	}
}

func (o *Object) unlock() {
	if threadsafeobject != 0 {
		threadsafeobjectmutexes[o.lockbucket()].Unlock()
	}
}

func (o *Object) runlock() {
	if threadsafeobject != 0 {
		threadsafeobjectmutexes[o.lockbucket()].RUnlock()
	}
}

func (o *Object) Absorb(source *Object) {
	o.AbsorbEx(source, false)
}

// Absorbs data and Pwn relationships from another object, sucking the soul out of it
// The sources empty shell should be discarded afterwards (i.e. not appear in an Objects collection)
func (o *Object) AbsorbEx(source *Object, fast bool) {
	if o == source {
		ui.Fatal().Msg("Can't absorb myself")
	}

	o.lock()
	if source.lockbucket() != o.lockbucket() {
		source.lock()
		defer source.unlock()
	}
	defer o.unlock()

	target := o

	// Fast mode does not merge values, it just relinks the source to the target

	if fast {
		// Just merge this one
		val := MergeValues(source.attr(DataSource), target.attr(DataSource))
		if val != nil {
			target.set(DataSource, val)
		}
	} else {
		source.AttrIterator(func(attr Attribute, values AttributeValues) bool {
			target.set(attr, MergeValues(target.attr(attr), values))
			return true
		})
	}

	for edgetarget, edges := range source.edges[Out] {
		target.edges[Out][edgetarget] = target.edges[Out][edgetarget].Merge(edges)
		delete(source.edges[Out], edgetarget)

		edgetarget.edges[In][target] = edgetarget.edges[In][target].Merge(edges)
		delete(edgetarget.edges[In], source)
	}

	for edgesource, edges := range source.edges[In] {
		target.edges[In][edgesource] = target.edges[In][edgesource].Merge(edges)
		delete(source.edges[In], edgesource)

		edgesource.edges[Out][target] = edgesource.edges[Out][target].Merge(edges)
		delete(edgesource.edges[Out], source)
	}

	for _, child := range source.children {
		target.adopt(child)
	}

	// If the source has a parent, but the target doesn't we assimilate that role (muhahaha)
	if source.parent != nil {
		if target.parent == nil {
			target.childOf(source.parent)
		}
		source.parent.removeChild(source)
		source.parent = nil
	}

	// Move the securitydescriptor, as we dont have the attribute saved to regenerate it (we throw it away at import after populating the cache)
	if source.sdcache != nil && target.sdcache != nil {
		// Both has a cache
		if !source.sdcache.Equals(target.sdcache) {
			// Different caches, so we need to merge them which is impossible
			ui.Warn().Msgf("Can not merge security descriptors between %v and %v", source.Label(), target.Label())
		}
	} else if target.sdcache == nil && source.sdcache != nil {
		target.sdcache = source.sdcache
	} else {
		target.sdcache = nil
	}

	target.objecttype = 0 // Recalculate this

	source.invalidated = true // Invalid object
}

func MergeValues(v1, v2 AttributeValues) AttributeValues {
	var val AttributeValues
	if v1.Len() == 0 {
		val = v2
	} else if v2.Len() == 0 {
		return nil
	} else if v1.Len() == 1 && v2.Len() == 1 {
		v1val := v1.First()
		v2val := v2.First()

		if CompareAttributeValues(v1val, v2val) {
			val = v1 // They're the same, so pick any
		} else {
			// They're not the same, join them
			val = AttributeValueSlice{v1val, v2val}
		}
	} else {
		// One or more of them have more than one value, do it the hard way
		var destinationSlice AttributeValueSlice
		var testvalues AttributeValues

		if v1.Len() > v2.Len() {
			destinationSlice = v1.Slice()
			testvalues = v2
		} else {
			destinationSlice = v2.Slice()
			testvalues = v1
		}

		resultingvalues := destinationSlice

		testvalues.Iterate(func(testvalue AttributeValue) bool {
			for _, existingvalue := range destinationSlice {
				if CompareAttributeValues(existingvalue, testvalue) { // Crap!!
					return false
				}
			}
			resultingvalues = append(resultingvalues, testvalue)
			return true
		})

		val = resultingvalues
	}
	return val
}

type StringMap map[string][]string

func (s StringMap) MarshalXML(e *xml.Encoder, start xml.StartElement) error {

	tokens := []xml.Token{start}

	for key, values := range s {
		t := xml.StartElement{Name: xml.Name{"", key}}
		for _, value := range values {
			tokens = append(tokens, t, xml.CharData(value), xml.EndElement{t.Name})
		}
	}

	tokens = append(tokens, xml.EndElement{start.Name})

	for _, t := range tokens {
		err := e.EncodeToken(t)
		if err != nil {
			return err
		}
	}

	// flush to ensure tokens are written
	return e.Flush()
}

func (o *Object) NameStringMap() StringMap {
	o.lock()
	defer o.unlock()
	result := make(StringMap)
	o.values.Iterate(func(attr Attribute, values AttributeValues) bool {
		result[attr.String()] = values.StringSlice()
		return true
	})
	return result
}

func (o *Object) MarshalJSON() ([]byte, error) {
	return jsoniter.ConfigCompatibleWithStandardLibrary.Marshal(o.NameStringMap())
}

func (o *Object) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	return o.NameStringMap().MarshalXML(e, start)
}

func (o *Object) IDString() string {
	return strconv.FormatUint(uint64(o.ID()), 10)
}

func (o *Object) DN() string {
	return o.OneAttrString(DistinguishedName)
}

var labelattrs = []Attribute{
	LDAPDisplayName,
	DisplayName,
	Name,
	DownLevelLogonName,
	SAMAccountName,
	Description,
	DistinguishedName,
	ObjectGUID,
	ObjectSid,
}

func (o *Object) Label() string {
	for _, attr := range labelattrs {
		val := o.OneAttrString(attr)
		if val != "" {
			return val
		}
	}
	return fmt.Sprintf("OBJ %v", o)
}

var primaryidattrs = []Attribute{
	DistinguishedName,
	ObjectGUID,
	ObjectSid, // Danger, Will Robinson
}

func (o *Object) PrimaryID() (Attribute, AttributeValue) {
	for _, attr := range primaryidattrs {
		if o.HasAttr(attr) {
			val := o.OneAttr(attr)
			if val != nil {
				return attr, val
			}
		}
	}
	return NonExistingAttribute, AttributeValueString("N/A")
}

func (o *Object) Type() ObjectType {
	if o.objecttype > 0 {
		return o.objecttype
	}

	category := o.Attr(ObjectCategorySimple)

	if category.Len() == 0 {
		return ObjectTypeOther
	}

	objecttype, found := ObjectTypeLookup(category.First().String())
	if found {
		o.objecttype = objecttype
	}
	return objecttype
}

func (o *Object) ObjectCategoryGUID(ao *Objects) uuid.UUID {
	// if o.objectcategoryguid == NullGUID {
	guid := o.OneAttrRaw(ObjectCategoryGUID)
	if guid == nil {
		return UnknownGUID
	}
	return guid.(uuid.UUID)
	// return o.objectcategoryguid
}

func (o *Object) AttrString(attr Attribute) []string {
	return o.Attr(attr).StringSlice()
}

func (o *Object) AttrRendered(attr Attribute) AttributeValues {
	if attr == ObjectCategory && o.HasAttr(ObjectCategorySimple) {
		return o.Attr(ObjectCategorySimple)
	}
	return o.Attr(attr)
}

func (o *Object) OneAttrRendered(attr Attribute) string {
	r := o.AttrRendered(attr)
	if r.Len() == 0 {
		return ""
	}
	return r.First().String()
}

// Returns synthetic blank attribute value if it isn't set
func (o *Object) get(attr Attribute) (AttributeValues, bool) {
	if o.invalidated {
		panic("object is invalidated")
	}
	if attributenums[attr].onget != nil {
		return attributenums[attr].onget(o, attr)
	}
	return o.values.Get(attr)
}

// Auto locking version
func (o *Object) Get(attr Attribute) (AttributeValues, bool) {
	o.rlock()
	defer o.runlock()
	return o.get(attr)
}

// Returns synthetic blank attribute value if it isn't set
func (o *Object) attr(attr Attribute) AttributeValues {
	if attrs, found := o.get(attr); found {
		if attrs == nil {
			panic(fmt.Sprintf("Looked for attribute %v and found NIL value", attr.String()))
		}
		return attrs
	}
	return NoValues{}
}

// Returns synthetic blank attribute value if it isn't set
func (o *Object) Attr(attr Attribute) AttributeValues {
	o.rlock()
	defer o.runlock()
	return o.attr(attr)
}

func (o *Object) OneAttrString(attr Attribute) string {
	o.rlock()
	defer o.runlock()
	a, found := o.get(attr)
	if !found {
		return ""
	}
	if a.Len() != 1 {
		ui.Error().Msgf("Attribute %v lookup for ONE value, but contains %v (%v)", attr.String(), a.Len(), strings.Join(a.StringSlice(), ", "))
	}
	return a.First().String()
}

func (o *Object) OneAttrRaw(attr Attribute) interface{} {
	a := o.Attr(attr)
	if a == nil {
		return nil
	}
	if a.Len() == 1 {
		return a.First().Raw()
	}
	return nil
}

func (o *Object) OneAttr(attr Attribute) AttributeValue {
	a := o.Attr(attr)
	if a == nil {
		return nil
	}
	if a.Len() == 1 {
		return a.First()
	}
	return nil
}

func (o *Object) HasAttr(attr Attribute) bool {
	_, found := o.Get(attr)
	return found
}

func (o *Object) HasAttrValue(attr Attribute, hasvalue AttributeValue) bool {
	var result bool
	o.Attr(attr).Iterate(func(value AttributeValue) bool {
		if CompareAttributeValues(value, hasvalue) {
			result = true
			return false
		}
		return true
	})
	return result
}

func (o *Object) AttrInt(attr Attribute) (int64, bool) {
	v, ok := o.OneAttrRaw(attr).(int64)
	return v, ok
}

func (o *Object) AttrTime(attr Attribute) (time.Time, bool) {
	v, ok := o.OneAttrRaw(attr).(time.Time)
	return v, ok
}

func (o *Object) AttrBool(attr Attribute) (bool, bool) {
	v, ok := o.OneAttrRaw(attr).(bool)
	return v, ok
}

/*
func (o *Object) AttrTimestamp(attr Attribute) (time.Time, bool) { // FIXME, switch to auto-time formatting
	v, ok := o.AttrInt(attr)
	if !ok {
		value := o.OneAttrString(attr)
		if strings.HasSuffix(value, "Z") { // "20171111074031.0Z"
			value = strings.TrimSuffix(value, "Z")  // strip "Z"
			value = strings.TrimSuffix(value, ".0") // strip ".0"
			t, err := time.Parse("20060102150405", value)
			return t, err == nil
		}
		return time.Time{}, false
	}
	t := util.FiletimeToTime(uint64(v))
	// ui.Debug().Msgf("Converted %v to %v", v, t)
	return t, true
}
*/

// Wrapper for Set - easier to call
func (o *Object) SetValues(a Attribute, values ...AttributeValue) {
	if values == nil {
		panic(fmt.Sprintf("tried to set attribute %v to NIL value", a.String()))
	}
	if len(values) == 0 {
		panic(fmt.Sprintf("tried to set attribute %v to NO values", a.String()))
	}
	if len(values) == 1 {
		o.Set(a, AttributeValueOne{values[0]})
	} else {
		o.Set(a, AttributeValueSlice(values))
	}
}

func (o *Object) SetFlex(flexinit ...interface{}) {
	o.lock()
	o.setFlex(flexinit...)
	o.unlock()
}

var avsPool sync.Pool

func init() {
	avsPool.New = func() interface{} {
		return make(AttributeValueSlice, 0, 16)
	}
}

func (o *Object) setFlex(flexinit ...interface{}) {
	var ignoreblanks bool

	attribute := NonExistingAttribute

	data := avsPool.Get().(AttributeValueSlice)

	for _, i := range flexinit {
		if i == IgnoreBlanks {
			ignoreblanks = true
			continue
		}
		switch v := i.(type) {
		case windowssecurity.SID:
			if ignoreblanks && v.IsNull() {
				continue
			}
			data = append(data, AttributeValueSID(v))
		case *[]string:
			if v == nil {
				continue
			}
			if ignoreblanks && len(*v) == 0 {
				continue
			}
			for _, s := range *v {
				if ignoreblanks && s == "" {
					continue
				}
				data = append(data, AttributeValueString(s))
			}
		case []string:
			if ignoreblanks && len(v) == 0 {
				continue
			}
			for _, s := range v {
				if ignoreblanks && s == "" {
					continue
				}
				data = append(data, AttributeValueString(s))
			}
		case *string:
			if v == nil {
				continue
			}
			if ignoreblanks && len(*v) == 0 {
				continue
			}
			data = append(data, AttributeValueString(*v))
		case string:
			if ignoreblanks && len(v) == 0 {
				continue
			}
			data = append(data, AttributeValueString(v))
		case *time.Time:
			if v == nil {
				continue
			}
			if ignoreblanks && v.IsZero() {
				continue
			}
			data = append(data, AttributeValueTime(*v))
		case time.Time:
			if ignoreblanks && v.IsZero() {
				continue
			}
			data = append(data, AttributeValueTime(v))
		case *bool:
			if v == nil {
				continue
			}
			data = append(data, AttributeValueBool(*v))
		case bool:
			data = append(data, AttributeValueBool(v))
		case int:
			if ignoreblanks && v == 0 {
				continue
			}
			data = append(data, AttributeValueInt(v))
		case int64:
			if ignoreblanks && v == 0 {
				continue
			}
			data = append(data, AttributeValueInt(v))
		case uint64:
			if ignoreblanks && v == 0 {
				continue
			}
			data = append(data, AttributeValueInt(v))
		case AttributeValue:
			if ignoreblanks && v.IsZero() {
				continue
			}
			data = append(data, v)
		case AttributeValueOne:
			data = append(data, v.Value)
		case []AttributeValue:
			for _, value := range v {
				if ignoreblanks && value.IsZero() {
					continue
				}
				data = append(data, value)
			}
		case AttributeValueSlice:
			for _, value := range v {
				if ignoreblanks && value.IsZero() {
					continue
				}
				data = append(data, value)
			}
		case NoValues:
			// Ignore it
		case Attribute:
			if attribute != NonExistingAttribute && (!ignoreblanks || len(data) > 0) {
				switch len(data) {
				case 0:
					o.set(attribute, NoValues{})
				case 1:
					o.set(attribute, AttributeValueOne{data[0]})
				default:
					newdata := make(AttributeValueSlice, len(data))
					copy(newdata, data)
					o.set(attribute, newdata)
				}
			}
			data = data[:0]
			attribute = v
		default:
			panic("SetFlex called with invalid type in object declaration")
		}
	}
	if attribute != NonExistingAttribute && (!ignoreblanks || len(data) > 0) {
		switch len(data) {
		case 0:
			o.set(attribute, NoValues{})
		case 1:
			o.set(attribute, AttributeValueOne{data[0]})
		default:
			newdata := make(AttributeValueSlice, len(data))
			copy(newdata, data)
			o.set(attribute, newdata)
		}
	}
	data = data[:0]
	avsPool.Put(data)
}

func (o *Object) Set(a Attribute, values AttributeValues) {
	o.lock()
	o.set(a, values)
	o.unlock()
}

func (o *Object) Clear(a Attribute) {
	o.lock()
	o.values.Clear(a)
	o.unlock()
}

func (o *Object) set(a Attribute, values AttributeValues) {
	if a.IsSingle() && values.Len() > 1 {
		ui.Warn().Msgf("Setting multiple values on non-multival attribute %v: %v", a.String(), strings.Join(values.StringSlice(), ", "))
	}

	if a == DownLevelLogonName {
		// There's been so many problems with DLLN that we're going to just check for these
		if values.Len() != 1 {
			ui.Warn().Msgf("Found DownLevelLogonName with multiple values: %v", strings.Join(values.StringSlice(), ", "))
		}
		for _, dlln := range values.StringSlice() {
			if dlln == "," {
				ui.Fatal().Msgf("Setting DownLevelLogonName to ',' is not allowed")
			}
			if dlln == "" {
				ui.Fatal().Msgf("Setting DownLevelLogonName to blank is not allowed")
			}
			if strings.HasPrefix(dlln, "S-") {
				ui.Warn().Msgf("DownLevelLogonName contains SID: %v", values.StringSlice())
			}
			if strings.HasSuffix(dlln, "\\") {
				ui.Fatal().Msgf("DownLevelLogonName %v ends with \\", dlln)
			}

			dotpos := strings.Index(dlln, ".")
			if dotpos >= 0 {
				backslashpos := strings.Index(dlln, "\\")
				if dotpos < backslashpos {
					ui.Warn().Msgf("DownLevelLogonName contains dot in domain: %v", dlln)
				}
			}
		}
	}

	if a == ObjectCategory || a == ObjectCategorySimple {
		// Clear objecttype cache attribute
		o.objecttype = 0
	}

	// Cache the NTSecurityDescriptor
	if a == NTSecurityDescriptor {
		if values.Len() != 1 {
			panic(fmt.Sprintf("Asked to set %v security descriptors on %v", values.Len(), o.Label()))
		} else {
			sd := values.First()
			if err := o.cacheSecurityDescriptor([]byte(sd.Raw().(string))); err != nil {
				ui.Error().Msgf("Problem parsing security descriptor for %v: %v", o.DN(), err)
			}
		}
		return // We dont store the raw version, just the decoded one, KTHX
	}

	// Deduplication of values
	switch vs := values.(type) {
	case AttributeValueSlice:
		if len(vs) == 1 {
			ui.Error().Msg("Wrong type")
		}
		for i, value := range vs {
			if value == nil {
				panic("tried to set nil value")
			}
			switch avs := value.(type) {
			case AttributeValueSID:
				vs[i] = AttributeValueSID(dedup.D.S(string(avs)))
			case AttributeValueString:
				vs[i] = AttributeValueString(dedup.D.S(string(avs)))
			case AttributeValueBlob:
				vs[i] = AttributeValueBlob(dedup.D.S(string(avs)))
			}
		}
	case AttributeValueOne:
		if vs.Value == nil {
			panic("tried to set nil value")
		}
		switch avs := vs.Value.(type) {
		case AttributeValueSID:
			vs.Value = AttributeValueSID(dedup.D.S(string(avs)))
		case AttributeValueString:
			vs.Value = AttributeValueString(dedup.D.S(string(avs)))
		case AttributeValueBlob:
			vs.Value = AttributeValueBlob(dedup.D.S(string(avs)))
		}
	}

	// if attributenums[a].onset != nil {
	// 	attributenums[a].onset(o, a, av)
	// 	o.values.Set(a, nil) // placeholder for iteration over attributes that are set
	// } else {
	o.values.Set(a, values)
	// }
}

func (o *Object) Meta() map[string]string {
	result := make(map[string]string)
	o.AttrIterator(func(attr Attribute, value AttributeValues) bool {
		if attr.String()[0] == '_' {
			result[attr.String()] = value.First().String()
		}
		return true
	})
	return result
}

func (o *Object) init() {
	o.id = atomic.AddUint32(&idcounter, 1)
	if o.edges[Out] == nil || o.edges[In] == nil {
		o.edges[Out] = make(EdgeConnections)
		o.edges[In] = make(EdgeConnections)
	}
}

func (o *Object) String() string {
	var result string
	result += "OBJECT " + o.DN() + "\n"
	o.AttrIterator(func(attr Attribute, values AttributeValues) bool {
		if attr == NTSecurityDescriptor {
			return true // continue
		}
		result += "  " + attributenums[attr].name + ":\n"
		for _, value := range values.Slice() {
			cleanval := stringsx.Clean(value.String())
			if cleanval != value.String() {
				result += fmt.Sprintf("    %v (%v original, %v cleaned)\n", value, len(value.String()), len(cleanval))
			} else {
				result += "    " + value.String() + "\n"
			}
		}

		if an, ok := values.(AttributeNode); ok {
			// dump with recursion - fixme
			an.Children()
		}
		return true // one more
	})
	return result
}

func (o *Object) StringACL(ao *Objects) string {
	result := o.String()

	sd, err := o.SecurityDescriptor()
	if err == nil {
		result += "----- SECURITY DESCRIPTOR DUMP -----\n"
		result += sd.String(ao)
	}
	result += "---------------\n"
	return result
}

// Dump the object to simple map type for debugging
func (o *Object) ValueMap() map[string][]string {
	result := make(map[string][]string)
	o.AttrIterator(func(attr Attribute, values AttributeValues) bool {
		result[attr.String()] = values.StringSlice()
		return true
	})
	return result
}

var ErrNoSecurityDescriptor = errors.New("no security desciptor")

// Return parsed security descriptor
func (o *Object) SecurityDescriptor() (*SecurityDescriptor, error) {
	if o.sdcache == nil {
		return nil, ErrNoSecurityDescriptor
	}
	return o.sdcache, nil
}

var ErrEmptySecurityDescriptorAttribute = errors.New("empty nTSecurityDescriptor attribute!?")

// Parse and cache security descriptor
func (o *Object) cacheSecurityDescriptor(rawsd []byte) error {
	if len(rawsd) == 0 {
		return ErrEmptySecurityDescriptorAttribute
	}

	securitydescriptorcachemutex.RLock()
	cacheindex := xxhash.Checksum64(rawsd)
	if sd, found := securityDescriptorCache[cacheindex]; found {
		securitydescriptorcachemutex.RUnlock()
		o.sdcache = sd
		return nil
	}
	securitydescriptorcachemutex.RUnlock()

	securitydescriptorcachemutex.Lock()
	sd, err := ParseSecurityDescriptor([]byte(rawsd))
	if err == nil {
		o.sdcache = &sd
		securityDescriptorCache[cacheindex] = &sd
	}

	securitydescriptorcachemutex.Unlock()
	return err
}

// Return the object's SID
func (o *Object) SID() windowssecurity.SID {
	if !o.sidcached {
		o.lock()
		o.sidcached = true
		if asid, ok := o.get(ObjectSid); ok {
			if asid.Len() == 1 {
				if sid, ok := asid.First().Raw().(windowssecurity.SID); ok {
					o.sid = sid
				}
			}
		}
		o.unlock()
	}
	sid := o.sid
	return sid
}

// Return the object's GUID
func (o *Object) GUID() uuid.UUID {
	o.lock()
	if !o.guidcached {
		o.guidcached = true
		if aguid, ok := o.get(ObjectGUID); ok {
			if aguid.Len() == 1 {
				if guid, ok := aguid.First().Raw().(uuid.UUID); ok {
					o.guid = guid
				}
			}
		}
	}
	guid := o.guid
	o.unlock()
	return guid
}

// Look up edge
func (o *Object) Edge(direction EdgeDirection, target *Object) EdgeBitmap {
	o.lock()
	bm := o.edges[direction][target]
	o.unlock()
	return bm
}

// Register that this object can pwn another object using the given method
func (o *Object) EdgeTo(target *Object, method Edge) {
	o.EdgeToEx(target, method, false)
}

// Enhanched Pwns function that allows us to force the pwn (normally self-pwns are filtered out)
func (o *Object) EdgeToEx(target *Object, method Edge, force bool) {
	if !force {
		if o == target { // SID check solves (some) dual-AD analysis problems
			// We don't care about self owns
			return
		}

		osid := o.SID()

		// Ignore these, SELF = self own, Creator/Owner always has full rights
		if osid == windowssecurity.SelfSID || osid == windowssecurity.SystemSID {
			return
		}

		tsid := target.SID()
		if !osid.IsBlank() && osid == tsid {
			return
		}
	}

	o.lock()
	o.edges[Out].Set(target, method) // Add the connection
	o.unlock()

	target.lock()
	target.edges[In].Set(o, method) // Add the reverse connection too
	target.unlock()
}

func (o *Object) EdgeClear(target *Object, method Edge) {
	o.lock()
	currentedge := o.edges[Out][target]
	if currentedge.IsSet(method) {
		o.edges[Out][target] = currentedge.Clear(method)
	}
	o.unlock()

	target.lock()
	currentedge = target.edges[In][o]
	if currentedge.IsSet(method) {
		target.edges[In][o] = currentedge.Clear(method)
	}
	target.unlock()
}

func (o *Object) EdgeCount(direction EdgeDirection) int {
	o.lock()
	result := len(o.edges[direction])
	o.unlock()
	return result
}

type ObjectEdge struct {
	o *Object
	e EdgeBitmap
}

func (o *Object) EdgeIterator(direction EdgeDirection, ei func(target *Object, edge EdgeBitmap) bool) {
	o.rlock()
	edges := make([]ObjectEdge, len(o.edges[direction]))
	var i int
	for target, edge := range o.edges[direction] {
		edges[i] = ObjectEdge{o: target, e: edge}
		i++
	}
	o.runlock()
	for _, objectedge := range edges {
		if !ei(objectedge.o, objectedge.e) {
			return
		}
	}
}

func (o *Object) EdgeIteratorRecursive(direction EdgeDirection, edgeMatch EdgeBitmap, af func(source, target *Object, edge EdgeBitmap, depth int) bool) {
	o.applyToObjectEdgesRecursive(direction, edgeMatch, af, make(map[*Object]struct{}), 1)
}

func (o *Object) applyToObjectEdgesRecursive(direction EdgeDirection, edgeMatch EdgeBitmap, af func(source, target *Object, edge EdgeBitmap, depth int) bool, appliedTo map[*Object]struct{}, depth int) {
	o.EdgeIterator(direction, func(target *Object, edge EdgeBitmap) bool {
		if _, found := appliedTo[target]; !found {
			edgeMatches := edge.Intersect(edgeMatch)
			if !edgeMatches.IsBlank() {
				appliedTo[target] = struct{}{}
				if af(o, target, edgeMatches, depth) {
					target.applyToObjectEdgesRecursive(direction, edgeMatch, af, appliedTo, depth+1)
				}
			}
		}
		return true
	})
}

func (o *Object) AttrIterator(f func(attr Attribute, avs AttributeValues) bool) {
	o.values.Iterate(f)
}

func (o *Object) ChildOf(parent *Object) {
	o.lock()
	if o.parent != nil {
		// Unlock, as we call thing that lock in the debug message
		o.unlock()
		ui.Debug().Msgf("Object %v already has %v as parent, so I'm not assigning %v as parent", o.Label(), o.parent.Label(), parent.Label())
		return
		o.lock()
		// panic("objects can only have one parent")
	}
	o.parent = parent
	o.unlock()
	parent.lock()
	parent.children = append(parent.children, o)
	parent.unlock()
}

func (o *Object) childOf(parent *Object) {
	if o.parent != nil {
		ui.Debug().Msgf("Object %v already has %v as parent, so I'm not assigning %v as parent", o.Label(), o.parent.Label(), parent.Label())
		return
	}
	o.parent = parent
	parent.children = append(parent.children, o)
}

func (o *Object) Adopt(child *Object) {
	o.lock()
	o.children = append(o.children, child)
	o.unlock()

	child.lock()
	if child.parent != nil {
		child.parent.lock()
		child.parent.removeChild(child)
		child.parent.unlock()
	}
	child.parent = o
	child.unlock()
}

func (o *Object) adopt(child *Object) {
	o.children = append(o.children, child)

	if child.parent != nil {
		child.parent.removeChild(child)
	}
	child.parent = o
}

func (o *Object) removeChild(child *Object) {
	for i, curchild := range o.children {
		if curchild == child {
			if len(o.children) == 1 {
				o.children = nil
			} else {
				o.children[i] = o.children[len(o.children)-1]
				o.children = o.children[:len(o.children)-1]
			}
			return
		}
	}
	panic("tried to remove a child not related to parent")
}

func (o *Object) Parent() *Object {
	o.rlock()
	defer o.runlock()
	return o.parent
}

func (o *Object) Children() []*Object {
	o.rlock()
	defer o.runlock()
	return o.children
}
