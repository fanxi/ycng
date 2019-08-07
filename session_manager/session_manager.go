/*
 * // Copyright (C) 2017 Yeecall authors
 * //
 * // This file is part of the Yecall library.
 *
 */

package session_manager

import (
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"encoding/json"
	"math/rand"

	"github.com/xujiajundd/ycng/relay"
	"github.com/xujiajundd/ycng/utils"
	"github.com/xujiajundd/ycng/utils/logging"
)

const (
	SessionManagerUserId = 0xffffffffffffffff
)

type SessionManager struct {
	sessions     map[uint64]*Session
	relays       []string
	saddr        string
	conn         *net.UDPConn
	subscriberCh chan *relay.ReceivedPacket
	dedup        *utils.LRU
	isRunning    bool
	lock         sync.RWMutex
	stop         chan struct{}
	wg           sync.WaitGroup
	ticker       *time.Ticker
}

func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions:     make(map[uint64]*Session),
		saddr:        ":20005",
		subscriberCh: make(chan *relay.ReceivedPacket),
		dedup:        utils.NewLRU(100, nil),
		isRunning:    false,
		stop:         make(chan struct{}),
		ticker:       time.NewTicker(200 * time.Second),
	}
	sm.GetRelays()
	return sm
}

func (sm *SessionManager) Start() {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	if !sm.isRunning {
		sm.isRunning = true
		sm.wg.Add(1)
		addr, err := net.ResolveUDPAddr("udp4", sm.saddr)
		if err != nil {
			logging.Logger.Error("error ResolveUDPAddr")
		}

		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			logging.Logger.Error("error ListenUDP", err)
			return
		}
		logging.Logger.Info("listen on port:", sm.saddr)

		sm.conn = conn

		sm.registerUserToRelays()

		go sm.loop()
		go sm.handleClient()
	}
}

func (sm *SessionManager) Stop() {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	if sm.isRunning {
		sm.isRunning = false
	}
	close(sm.stop)
}

func (sm *SessionManager) WaitForShutdown() {
	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigc)
		<-sigc
		sm.Stop()
	}()

	sm.wg.Wait()
	return
}

func (sm *SessionManager) loop() {
	defer sm.wg.Done()

	for {
		select {
		case <-sm.stop:
			return
		case packet := <-sm.subscriberCh:
			sm.handlePacket(packet)
		case time := <-sm.ticker.C:
			sm.handleTicker(time)
		}
	}
}

func (sm *SessionManager) handleClient() {
	var buf [2048]byte

	for {
		size, addr, err := sm.conn.ReadFromUDP(buf[0:])
		if err != nil {
			logging.Logger.Error("error ReadFromUDP ", err)
			continue
		}

		data := make([]byte, size)
		copy(data, buf[0:size])
		packet := &relay.ReceivedPacket{
			Body:        data,
			FromUdpAddr: addr,
			Time:        time.Now().UnixNano(),
		}

		sm.subscriberCh <- packet
	}
}

func (sm *SessionManager) handlePacket(packet *relay.ReceivedPacket) {
	msg, err := relay.NewMessageFromObfuscatedData(packet.Body)
	if err != nil {
		logging.Logger.Warn("error:", err)
		return
	}

	switch msg.MsgType {
	case relay.UdpMessageTypeUserRegReceived:
		logging.Logger.Info("user reg received from ", packet.FromUdpAddr)
	case relay.UdpMessageTypeUserSignal:
		sm.handleMessageUserSignal(msg)
	default:
		logging.Logger.Warn("unrecognized message type")
	}
}

func (sm *SessionManager) handleTicker(now time.Time) {
	//每隔200秒重新注册一次
	sm.registerUserToRelays()

	//清理已经结束的session，1-1有收到过end，多方发出或收到所有的end。或者sm主动轮询参与者？
}

func (sm *SessionManager) handleMessageUserSignal(msg *relay.Message) {
	//去重
	if sm.dedup.Contains(string(msg.Payload)) {
		return
	} else {
		sm.dedup.Add(string(msg.Payload), true)
	}

	//Unmarshal
	signal := NewSignalTemp()
	err := signal.Unmarshal(msg.Payload)
	if err != nil {
		logging.Logger.Warn("signal unmarshal error:", err)
		return
	}

	/*
	  1. 1-1和多方第一个人，都必须先请求sid。多方其他人可以通过呼出或者通过邀请呼入，那时已经有sid
	  2. 收到请求sid时，即创建session，并回复sid
	  3. 其他信令如果sid为0或者session不存在，都是错误
	  4. 如果信令的To不是SessionManager，那么是1-1模式
	  5. 1-1模式下，透明转发信令，但维护参与方的状态
	  6. 1-1模式下，如果收到member_op，转为多方模式。多方模式不能再转回1-1模式
	  7. 多方模式下，所有的状态变化，session manager要发member state给所有参与者
	*/

	if signal.Signal == YCKCallSignalTypeSidRequest {
		//生成一个与现存不重复的sid
		var sid uint64
		for {
			sid = rand.Uint64()
			if sm.sessions[sid] == nil {
				break
			}
		}
		//创建session
		session := NewSession(sid)
		sm.sessions[sid] = session

		//回复信令
		sid_created := NewSignal(YCKCallSignalTypeSidCreated, SessionManagerUserId, signal.From, sid)
		payload, err := sid_created.Marshal()
		if err == nil {
			msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, signal.From, 0, payload, nil)
			sm.sendSignalMessage(msg)
		} else {
			logging.Logger.Warn("signal marshal error:", err)
		}
		return
	}

	if signal.SessionId == 0 {
		logging.Logger.Warn("error signal:", signal.Signal, " with sid=0 ", signal.From, signal.To)
		return
	}

	session := sm.sessions[signal.SessionId]
	if session == nil {
		logging.Logger.Warn("session not existed for ", signal)
		return
	}

	if signal.To != SessionManagerUserId {
		//1-1信令，直接转发signal, 维护参与者状态
		if session.Mode == YCKCallModeMultiple { //进入多方模式后，不能再接受1-1信令
			logging.Logger.Warn("receive 1-1 signal when in multipart mode")
			return
		} else {
			session.Mode = YCKCallModeOneToOne
		}

		payload, err := signal.Marshal()
		if err == nil {
			msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, signal.To, 0, payload, nil)
			sm.sendSignalMessage(msg)
		} else {
			logging.Logger.Warn("signal marshal error:", err)
			return
		}

		pf := session.Participants[signal.From]
		pt := session.Participants[signal.To]

		switch signal.Signal {
		case YCKCallSignalTypeInvite:
			if pf == nil {
				pf = NewParticipant(signal.From)
				session.Participants[signal.From] = pf
			}
			if pt == nil {
				pt = NewParticipant(signal.To)
				session.Participants[signal.To] = pt
			}
			if pf.InState(YCKParticipantStateIdle) {
				pf.SetState(YCKParticipantStateCalling)
				pt.SetState(YCKParticipantStateCalled)
				pf.SetEvent(YCKParticipantEventInvite)
				pt.SetEvent(YCKParticipantEventRecvInvite)
			}
		case YCKCallSignalTypeCancel:
			if pf != nil && (pf.InState(YCKParticipantStateCalling) || pf.InState(YCKParticipantStateIncall)) {
				pf.SetState(YCKParticipantStateIdle)
				pt.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventCancel)
				pt.SetEvent(YCKParticipantEventRecvCancel)
			}
		case YCKCallSignalTypeAccept:
			if pf != nil && pf.InState(YCKParticipantStateCalled) {
				pf.SetState(YCKParticipantStateIncall)
				pt.SetState(YCKParticipantStateIncall)
				pf.SetEvent(YCKParticipantEventAccept)
				pt.SetEvent(YCKParticipantEventRecvAccept)
			}
		case YCKCallSignalTypeReject:
			if pf != nil && pf.InState(YCKParticipantStateCalled) {
				pf.SetState(YCKParticipantStateIdle)
				pt.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventReject)
				pt.SetEvent(YCKParticipantEventRecvReject)
			}
		case YCKCallSignalTypeBusy:
			if pf != nil && pf.InState(YCKParticipantStateCalled) {
				pf.SetState(YCKParticipantStateIdle)
				pt.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventBusy)
				pt.SetEvent(YCKParticipantEventRecvBusy)
			}
		case YCKCallSignalTypeEnd:
			if pf != nil {
				pf.SetState(YCKParticipantStateIdle)
				pt.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventEnd)
				pt.SetEvent(YCKParticipantEventRecvEnd)
			}
		default:

		}
	} else {
		//管理session，member状态
		if session.Mode == YCKCallModeOneToOne && signal.Signal != YCKCallSignalTypeMemberOp {
			logging.Logger.Warn("multipart signal ignored in 1-1 mode ", signal.From, signal.To, signal.Signal)
			return
		}

		if session.Mode == YCKCallModeUndecided {
			session.Mode = YCKCallModeMultiple
		}

		pf := session.Participants[signal.From]

		switch signal.Signal {
		case YCKCallSignalTypeInvite:
			//回复ring，accept，设置状态为incall
			if pf == nil {
				pf = NewParticipant(signal.From)
				session.Participants[signal.From] = pf
			}
			if pf.InState(YCKParticipantStateIdle) {
				pf.SetState(YCKParticipantStateCalling)
				pf.SetEvent(YCKParticipantEventInvite)

				ring := NewSignal(YCKCallSignalTypeRing, SessionManagerUserId, signal.From, session.Sid)
				payload, err := ring.Marshal()
				if err == nil {
					msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, signal.From, 0, payload, nil)
					sm.sendSignalMessage(msg)
				} else {
					logging.Logger.Warn("signal marshal error:", err)
				}

				accept := NewSignal(YCKCallSignalTypeAccept, SessionManagerUserId, signal.From, session.Sid)
				payload, err = accept.Marshal()
				if err == nil {
					msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, signal.From, 0, payload, nil)
					sm.sendSignalMessage(msg)
				} else {
					logging.Logger.Warn("signal marshal error:", err)
				}
				pf.SetState(YCKParticipantStateIncall)
				pf.SetEvent(YCKParticipantEventRecvAccept)
			}
		case YCKCallSignalTypeCancel: //calling这个状态其实并不存在
			if pf != nil && (pf.InState(YCKParticipantStateCalling) || pf.InState(YCKParticipantStateIncall)) {
				pf.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventCancel)
			}
		case YCKCallSignalTypeEnd:
			if pf != nil {
				pf.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventEnd)
			}
		case YCKCallSignalTypeAccept:
			if pf != nil && pf.InState(YCKParticipantStateCalled) {
				pf.SetState(YCKParticipantStateIncall)
				pf.SetEvent(YCKParticipantEventAccept)
			}
		case YCKCallSignalTypeReject:
			if pf != nil && pf.InState(YCKParticipantStateCalled) {
				pf.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventReject)
			}
		case YCKCallSignalTypeBusy:
			if pf != nil && pf.InState(YCKParticipantStateCalled) {
				pf.SetState(YCKParticipantStateIdle)
				pf.SetEvent(YCKParticipantEventBusy)
			}
		case YCKCallSignalTypeMemberOp:
			if session.Mode == YCKCallModeOneToOne { //1-1模式时收到多方信令则转入多方模式，并且要通知所有参与方改模式
				session.Mode = YCKCallModeMultiple
				logging.Logger.Info("change to multipart mode")
			}

			op, okOp := signal.Info["op"].(string)
			members, okMem := signal.Info["members"].([]interface{})
			if okOp && okMem {
				if op == "invite" {
					for _, value := range members {
						mem, err := strconv.ParseUint(value.(json.Number).String(), 10, 64)
						if err == nil {
							p := session.Participants[mem]
							if p == nil {
								p = NewParticipant(mem)
								session.Participants[mem] = p
							}
							if p.InState(YCKParticipantStateIdle) {
								p.SetState(YCKParticipantStateCalled)
								p.SetEvent(YCKParticipantEventRecvInvite)

								invite := NewSignal(YCKCallSignalTypeInvite, SessionManagerUserId, mem, session.Sid)
								//TODO:invite将来要加更多内容，比如relays，device info等等

								payload, err := invite.Marshal()
								if err == nil {
									msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, mem, 0, payload, nil)
									sm.sendSignalMessage(msg)
								} else {
									logging.Logger.Warn("signal marshal error:", err)
								}
							} else {
								logging.Logger.Warn("member ", p, " not in idle state, cannot invite")
							}
						} else {
							logging.Logger.Warn("parseUint error ", err)
						}
					}
				} else if op == "kick" {
					for _, value := range members {
						mem, err := strconv.ParseUint(value.(json.Number).String(), 10, 64)
						if err == nil {
							p := session.Participants[mem]
							if p == nil {
								p = NewParticipant(mem)
								session.Participants[mem] = p
							}
							if p.InState(YCKParticipantStateIncall) {
								p.SetState(YCKParticipantStateIdle)
								p.SetEvent(YCKParticipantEventRecvEnd)

								end := NewSignal(YCKCallSignalTypeEnd, SessionManagerUserId, mem, session.Sid)
								payload, err := end.Marshal()
								if err == nil {
									msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, mem, 0, payload, nil)
									sm.sendSignalMessage(msg)
								} else {
									logging.Logger.Warn("signal marshal error:", err)
								}
							} else {
								logging.Logger.Warn("member ", p, " not in incall state, cannot kick")
							}
						} else {
							logging.Logger.Warn("parseUint error ", err)
						}
					}
				} else {
					logging.Logger.Warn("unrecognized member op cmd ", op)
				}
			} else {
				logging.Logger.Warn("member op cmd error ", op, members)
			}

		default:

		}

		//把状态通知所有参与方, 这个消息需要push么？
		info := make(map[string]interface{})
		pState := make(map[string]map[string]uint16)
		for _, p := range session.Participants {
           key := strconv.FormatUint(p.Uid, 10)
           value := make(map[string]uint16)
           value["state"] = p.State
           value["event"] = p.Event
           pState[key] = value
		}
		info["state"] = pState
		for _, p := range session.Participants {
			state := NewSignal(YCKCallSignalTypeMemberState, SessionManagerUserId, p.Uid, session.Sid)
			state.Info = info
			payload, err := state.Marshal()
			if err == nil {
				msg := relay.NewMessage(relay.UdpMessageTypeUserSignal, SessionManagerUserId, p.Uid, 0, payload, nil)
				sm.sendSignalMessage(msg)
			} else {
				logging.Logger.Warn("signal marshal error:", err)
			}
		}
	}
}

func (sm *SessionManager) registerUserToRelays() {
	msg := relay.NewMessage(relay.UdpMessageTypeUserReg,
		SessionManagerUserId, 0, 0, nil, nil)
	sm.sendSignalMessageByRelays(msg)
}

func (sm *SessionManager) sendSignalMessageByRelays(msg *relay.Message) {
	data := msg.ObfuscatedDataOfMessage()

	for _, relay := range sm.relays {
		udpAddr, err := net.ResolveUDPAddr("udp4", relay)
		if err != nil {
			logging.Logger.Error("incorrect addr ", err)
		}

		_, err = sm.conn.WriteToUDP(data, udpAddr)
		if err != nil {
			logging.Logger.Error("udp write error", err)
		}
	}
}

func (sm *SessionManager) sendSignalMessage(msg *relay.Message) {
	sm.sendSignalMessageByRelays(msg)
	//todo：通过push平台再发
}

func (sm *SessionManager) GetRelays() {
	sm.relays = []string{
		"10.18.96.46:19001",
		"10.18.96.46:19002",
		"10.18.96.46:19003",
		"106.75.106.193:19001",
		"117.50.61.49:19001",
		"117.50.63.224:19001",
	}
}
