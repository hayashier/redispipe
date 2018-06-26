package redis

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
)

type ClusterSlotMoving byte

const (
	ClusterSlotMigrating ClusterSlotMoving = 1
	ClusterSlotImporting ClusterSlotMoving = 2
)

type SlotsRange struct {
	From  int
	To    int
	Addrs []string
}

func ParseSlotsInfo(res interface{}) ([]SlotsRange, error) {
	const NumSlots = 1 << 14
	if err := AsError(res); err != nil {
		return nil, err
	}

	errf := func(f string, args ...interface{}) ([]SlotsRange, error) {
		msg := fmt.Sprintf(f, args...)
		err := NewErrMsg(ErrKindResponse, ErrResponseUnexpected, msg)
		return nil, err
	}

	var rawranges []interface{}
	var ok bool
	if rawranges, ok = res.([]interface{}); !ok {
		return errf("type is not array: %+v", res)
	}

	ranges := make([]SlotsRange, len(rawranges))
	for i, rawelem := range rawranges {
		var rawrange []interface{}
		var ok bool
		var i64 int64
		r := SlotsRange{}
		if rawrange, ok = rawelem.([]interface{}); !ok || len(rawrange) < 3 {
			return errf("format mismatch: res[%d]=%+v", i, rawelem)
		}
		if i64, ok = rawrange[0].(int64); !ok || i64 < 0 || i64 >= NumSlots {
			return errf("format mismatch: res[%d][0]=%+v", i, rawrange[0])
		}
		r.From = int(i64)
		if i64, ok = rawrange[1].(int64); !ok || i64 < 0 || i64 >= NumSlots {
			return errf("format mismatch: res[%d][1]=%+v", i, rawrange[1])
		}
		r.To = int(i64)
		if r.From > r.To {
			return errf("range wrong: res[%d]=%+v (%+v)", i, rawrange)
		}
		for j := 2; j < len(rawrange); j++ {
			rawaddr, ok := rawrange[j].([]interface{})
			if !ok || len(rawaddr) < 2 {
				return errf("address format mismatch: res[%d][%d] = %+v",
					i, j, rawrange[j])
			}
			host, ok := rawaddr[0].([]byte)
			port, ok2 := rawaddr[1].(int64)
			if !ok || !ok2 || port <= 0 || port+10000 > 65535 {
				return errf("address format mismatch: res[%d][%d] = %+v",
					i, j, rawaddr)
			}
			r.Addrs = append(r.Addrs, string(host)+":"+strconv.Itoa(int(port)))
		}
		sort.Strings(r.Addrs[1:])
		ranges[i] = r
	}
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].From < ranges[j].From
	})
	return ranges, nil
}

type ClusterInstanceInfo struct {
	Uuid      string
	IpPort    string
	Ip        string
	Port      int
	Port2     int
	Fail      bool
	MySelf    bool
	SlaveOf   string
	Slots     [][2]uint16
	Migrating []ClusterSlotMigration
}

type ClusterInstanceInfos []ClusterInstanceInfo

type ClusterSlotMigration struct {
	Number uint16
	Moving ClusterSlotMoving
	Peer   string
}

func (ii *ClusterInstanceInfo) IsMaster() bool {
	return ii.SlaveOf == ""
}

func (iis ClusterInstanceInfos) HashSum() uint64 {
	hsh := fnv.New64a()
	for _, ii := range iis {
		fmt.Fprintf(hsh, "%s\t%s\t%d\t%v\t%s", ii.Uuid, ii.IpPort, ii.Port2, ii.Fail, ii.SlaveOf)
		for _, slots := range ii.Slots {
			fmt.Fprintf(hsh, "\t%d-%d", slots[0], slots[1])
		}
		hsh.Write([]byte("\n"))
	}
	return hsh.Sum64()
}

func (iis ClusterInstanceInfos) CollectAddressesAndMigrations(addrs map[string]struct{}, migrating map[uint16]struct{}) {
	for _, ii := range iis {
		if ii.Ip > "" && ii.Port != 0 {
			addrs[ii.IpPort] = struct{}{}
		}
		for _, m := range ii.Migrating {
			migrating[m.Number] = struct{}{}
		}
	}
}

func (iis ClusterInstanceInfos) SlotsRanges() []SlotsRange {
	uuid2addrs := make(map[string][]string)
	for _, ii := range iis {
		if ii.IsMaster() {
			uuid2addrs[ii.Uuid] = append([]string{ii.IpPort}, uuid2addrs[ii.Uuid]...)
		} else {
			uuid2addrs[ii.SlaveOf] = append(uuid2addrs[ii.SlaveOf], ii.IpPort)
		}
	}
	ranges := make([]SlotsRange, 0, 16)
	for _, ii := range iis {
		if !ii.IsMaster() || len(ii.Slots) == 0 {
			continue
		}
		for _, slots := range ii.Slots {
			ranges = append(ranges, SlotsRange{
				From:  int(slots[0]),
				To:    int(slots[1]),
				Addrs: uuid2addrs[ii.Uuid],
			})
		}
	}
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].From < ranges[j].From
	})
	return ranges
}

func ParseClusterInfo(res interface{}) (ClusterInstanceInfos, error) {
	var err error
	if err = AsError(res); err != nil {
		return nil, err
	}

	errf := func(f string, args ...interface{}) (ClusterInstanceInfos, error) {
		msg := fmt.Sprintf(f, args...)
		err := NewErrMsg(ErrKindResponse, ErrResponseUnexpected, msg)
		return nil, err
	}

	infob, ok := res.([]byte)
	if !ok {
		return errf("type is not []bytes, but %t", res)
	}
	info := string(infob)
	lines := strings.Split(info, "\n")
	infos := ClusterInstanceInfos{}
	for _, line := range lines {
		if len(line) < 16 {
			continue
		}
		parts := strings.Split(line, " ")
		ipp := strings.Split(parts[1], "@")
		addrparts := strings.Split(ipp[0], ":")
		if len(ipp) != 2 || len(addrparts) != 2 {
			return errf("ip-port is not in 'ip:port@port2' format, but %q", line)
		}
		node := ClusterInstanceInfo{
			Uuid:   parts[0],
			IpPort: ipp[0],
		}
		node.Ip = addrparts[0]
		node.Port, _ = strconv.Atoi(addrparts[1])
		node.Port2, _ = strconv.Atoi(ipp[1])

		node.Fail = strings.Contains(parts[2], "fail")
		if strings.Contains(parts[2], "slave") {
			node.SlaveOf = parts[3]
		}

		for _, slot := range parts[8:] {
			if slot[0] == '[' {
				var uuid string
				var slotn int
				dir := ClusterSlotImporting

				if ix := strings.Index(slot, "-<-"); ix != -1 {
					slotn, err = strconv.Atoi(slot[1:ix])
					if err != nil {
						return errf("slot number is not an integer: %q", slot[1:ix])
					}
					uuid = slot[ix+3 : len(slot)-1]
				} else if ix = strings.Index(slot, "->-"); ix != -1 {
					slotn, err = strconv.Atoi(slot[1:ix])
					if err != nil {
						return errf("slot number is not an integer: %q", slot[1:ix])
					}
					uuid = slot[ix+3 : len(slot)-1]
					dir = ClusterSlotMigrating
				}
				migrating := ClusterSlotMigration{
					Number: uint16(slotn),
					Moving: dir,
					Peer:   uuid,
				}
				node.Migrating = append(node.Migrating, migrating)
			} else if ix := strings.IndexByte(slot, '-'); ix != -1 {
				from, err := strconv.Atoi(slot[:ix])
				if err != nil {
					return errf("slot number is not an integer: %q", slot)
				}
				to, err := strconv.Atoi(slot[ix+1:])
				if err != nil {
					return errf("slot number is not an integer: %q", slot)
				}
				node.Slots = append(node.Slots, [2]uint16{uint16(from), uint16(to)})
			} else {
				slotn, err := strconv.Atoi(slot)
				if err != nil {
					return errf("slot number is not an integer: %q", slot)
				}
				node.Slots = append(node.Slots, [2]uint16{uint16(slotn), uint16(slotn)})
			}
		}
		infos = append(infos, node)
	}
	sort.Slice(infos, func(i, j int) bool {
		// masters first: it will help further
		if infos[i].IsMaster() && !infos[j].IsMaster() {
			return true
		}
		if !infos[i].IsMaster() && infos[j].IsMaster() {
			return false
		}
		return infos[i].Uuid < infos[i].Uuid
	})
	return infos, nil
}