package client

/*
#cgo CFLAGS: -g -Wall -I/sonic/src/sonic-swss-common/common -Wformat -Werror=format-security -fPIE 
#cgo LDFLAGS: -lpthread -lboost_thread -lboost_system -lzmq -lboost_serialization -luuid -lswsscommon
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>
#include "events_wrap.h"
*/
import "C"

import (
    "encoding/json"
    "fmt"
    "sync"
    "time"
    "unsafe"

    "github.com/go-redis/redis"

    spb "github.com/sonic-net/sonic-gnmi/proto"
    "github.com/Workiva/go-datastructures/queue"
    log "github.com/golang/glog"
    gnmipb "github.com/openconfig/gnmi/proto/gnmi"
)

const SUBSCRIBER_TIMEOUT = (2 * 1000)  // 2 seconds
const HEARTBEAT_TIMEOUT = 2
const EVENT_BUFFSZ = 4096

const LATENCY_LIST_SIZE = 10  // Size of list of latencies.
const PQ_MAX_SIZE = 10240     // Max cnt of pending events in PQ

// STATS counters
const MISSED = "EVENTS_COUNTERS:missed_internal"
const DROPPED = "EVENTS_COUNTERS:missed_by_slow_receiver"
const LATENCY = "EVENTS_COUNTERS:latency_in_ms"

var STATS_CUMULATIVE_KEYS = [...]string {MISSED, DROPPED}
var STATS_ABSOLUTE_KEYS = [...]string {LATENCY}

const STATS_FIELD_NAME = "value"


type EventClient struct {

    prefix      *gnmipb.Path
    path        *gnmipb.Path

    q           *queue.PriorityQueue
    channel     chan struct{}

    wg          *sync.WaitGroup // wait for all sub go routines to finish

    subs_handle unsafe.Pointer

    stopped     int

    // Stats counter
    counters    map[string]uint64

    last_latencies  [LATENCT_LIST_SIZE]int64
    last_latency_index  int
    last_latency_full   bool

    last_errors int64
}

func NewEventClient(paths []*gnmipb.Path, prefix *gnmipb.Path, logLevel int) (Client, error) {
    var evtc EventClient
    evtc.prefix = prefix
    for _, path := range paths {
        // Only one path is expected. Take the last if many
        evtc.path = path
    }
    C.swssSetLogPriority(C.int(logLevel))

    /* Init subscriber with cache use and defined time out */
    evtc.subs_handle = C.events_init_subscriber_wrap(true, C.int(SUBSCRIBER_TIMEOUT))
    evtc.stopped = 0

    /* Init list & counters */
    for _, key := range STATS_CUMULATIVE_KEYS {
        evtc.counters[key] = 0
    }

    for _, key := range STATS_ABSOLUTE_KEYS {
        evtc.counters[key] = 0
    }

    for i := 0; i < len(evtc.last_latencies); i++ {
        evtc.last_latencies[i] = 0
    }
    evtc.last_latency_index = 0
    evtc.last_errors = 0
    evtc.last_latency_full = false

    log.V(7).Infof("NewEventClient constructed. logLevel=%d", logLevel)

    return &evtc, nil
}


func update_stats(evtc *EventClient) {
    ns := sdcfg.GetDbDefaultNamespace()
    rclient := redis.NewClient(&redis.Options{
        Network:    "tcp",
        Addr:       sdcfg.GetDbTcpAddr("COUNTERS_DB", ns),
        Password:   "", // no password set,
        DB:         sdcfg.GetDbId("COUNTERS_DB", ns),
        DialTimeout:0,
    })

    var db_counters map[string]uint64
    var wr_counters *map[string]uint64 = NULL

    /* Init current values for cumulative keys and clear for absolute */
    for _, key := range STATS_CUMULATIVE_KEYS {
        fv, err := rclient.HGetAll(key).Result()
        db_counters[key] = fv[STATS_FIELD_NAME]
    }
    for _, key := range STATS_ABSOLUTE_KEYS {
        db_counters[key] = 0
    }

    for evtc.stopped == 0 {
        var tmp_counters map[string]uint64

        // compute latency
        if evtc.last_latency_full {
            var total uint64 = 0
            var cnt int = 0

            for _, v := range evtc.last_latencies {
                if v > 0 {
                    total += v
                    cnt++
                }
            }
            evtc.counters[LATENCY] = (int64) (total/cnt/1000/1000)
        }

        for key, val := range evtc.counters {
            tmp_counters[key] = val + db_counters[key]
        }
        tmp_counters[DROPPED] += evtc.last_errors

        if (wr_counters != nil) && !reflect.DeepEqual(tmp_counters, *wr_counters) {
            for key, val := range tmp_counters {
                sval := strconv.FormatUint(val, 10)
                fv := map[string]string { STATS_FIELD_NAME : sval }
                err := rclient.HMSet(key, fv)
                if err != nil {
                    log.V(3).Infof("EventClient failed to update COUNTERS key:%s val:%v err:%v",
                    key, fv, err)
                }
            }
            wr_counters = &tmp_counters
        }
        time.Sleep(time.Second)
    }
}


// String returns the target the client is querying.
func (evtc *EventClient) String() string {
    return fmt.Sprintf("EventClient Prefix %v", evtc.prefix.GetTarget())
}


func get_events(evtc *EventClient) {
    str_ptr := C.malloc(C.sizeof_char * C.size_t(EVENT_BUFFSZ)) 
    defer C.free(unsafe.Pointer(str_ptr))

    evt_ptr := &C.event_receive_op_C_t{}
    evt_ptr = (*C.event_receive_op_C_t)(C.malloc(C.size_t(unsafe.Sizeof(C.event_receive_op_C_t{}))))
    defer C.free(unsafe.Pointer(evt_ptr))

    evt_ptr.event_str = (*C.char)(str_ptr)
    evt_ptr.event_sz = C.uint32(EVENT_BUFFSZ)

    for {

        rc := C.event_receive_wrap(evtc.subs_handle, evt_ptr)
        log.V(7).Infof("C.event_receive_wrap rc=%d evt:%s", rc, (*C.char)(evt_ptr))

        if rc == 0 {
            evtc.counters[MISSED] += evt_ptr.missed_cnt
            if evtc.q.len() < PQ_MAX_SIZE {
                evtTv := &gnmipb.TypedValue {
                    Value: &gnmipb.TypedValue_StringVal {
                        StringVal: C.GoString((*C.char)(evt_ptr.event_str)),
                    }}
                if err := send_event(evtc, evtTv,
                        C.int64_t(evt_ptr.publish_epoch_ms)); err != nil {
                    return
                }
            } else {
                evtc.counters[DROPPED] += 1
            }
        }
        if evtc.stopped == 1 {
            log.V(1).Infof("%v stop channel closed, exiting get_events routine", evtc)
            C.events_deinit_subscriber_wrap(evtc.subs_handle)
            evtc.subs_handle = nil
            return
        }
        // TODO: Record missed count in stats table.
        // intVar, err := strconv.Atoi(C.GoString((*C.char)(c_mptr)))
    }
}


func send_event(evtc *EventClient, tv *gnmipb.TypedValue,
        timestamp int64) error {
    spbv := &spb.Value{
        Prefix:    evtc.prefix,
        Path:    evtc.path,
        Timestamp: timestamp,
        Val:  tv,
    }

    log.V(7).Infof("Sending spbv")
    if err := evtc.q.Put(Value{spbv}); err != nil {
        log.V(3).Infof("Queue error:  %v", err)
        return err
    }
    log.V(7).Infof("send_event done")
    return nil
}

func (evtc *EventClient) StreamRun(q *queue.PriorityQueue, stop chan struct{}, wg *sync.WaitGroup, subscribe *gnmipb.SubscriptionList) {
    hbData := make(map[string]interface{})
    hbData["heart"] = "beat"
    hbVal, _ := json.Marshal(hbData)

    hbTv := &gnmipb.TypedValue {
        Value: &gnmipb.TypedValue_JsonIetfVal{
            JsonIetfVal: hbVal,
        }}


    evtc.wg = wg
    defer evtc.wg.Done()

    evtc.q = q
    evtc.channel = stop

    go get_events(evtc)

    for {
        select {
        case <-IntervalTicker(time.Second * HEARTBEAT_TIMEOUT):
            log.V(7).Infof("Ticker received")
            if err := send_event(evtc, hbTv, time.Now().UnixNano()); err != nil {
                return
            }
        case <-evtc.channel:
            evtc.stopped = 1
            log.V(3).Infof("Channel closed by client")
            return
        }
    }
}


func (evtc *EventClient) Get(wg *sync.WaitGroup) ([]*spb.Value, error) {
    return nil, nil
}

func (evtc *EventClient) OnceRun(q *queue.PriorityQueue, once chan struct{}, wg *sync.WaitGroup, subscribe *gnmipb.SubscriptionList) {
    return
}

func (evtc *EventClient) PollRun(q *queue.PriorityQueue, poll chan struct{}, wg *sync.WaitGroup, subscribe *gnmipb.SubscriptionList) {
    return
}


func (evtc *EventClient) Close() error {
    return nil
}

func  (evtc *EventClient) Set(delete []*gnmipb.Path, replace []*gnmipb.Update, update []*gnmipb.Update) error {
    return nil
}
func (evtc *EventClient) Capabilities() []gnmipb.ModelData {
    return nil
}

func (c *EventClient) sent(val *spb.Value) {
    c.last_latencies[c.last_latency_index] = time.Now().UnixNano() - val.Timestamp
    c.last_latency_index += 1
    if c.last_latency_index >= len(c.last_latencies) {
        c.last_latency_index = 0
        c.last_latency_full = true
    }
}

func (c *EventClient) failed_send(cnt int64) {
    c.last_errors = cnt
}


// cgo LDFLAGS: -L/sonic/target/files/bullseye -lxswsscommon -lpthread -lboost_thread -lboost_system -lzmq -lboost_serialization -luuid -lxxeventxx -Wl,-rpath,/sonic/target/files/bullseye

