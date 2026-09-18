// Package store 提供仅追加（append-only）事件日志与从事件重放出来的内存投影。
//
// 审计要求：
//   - 事件一旦追加不可修改、不可删除；每条记录携带序号与对前一条记录的 SHA-256 哈希，
//     形成哈希链，任何篡改都会在 Verify 或重放时暴露；
//   - 重启后系统从事件日志重放恢复全部状态（含提醒已发幂等记录与暂停累计）；
//     外部提醒投递失败由 service 层的未送达队列在下次提醒泵中按序补发。
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"example.com/09181/q012/internal/domain"
)

// Envelope 事件日志中的一行。
type Envelope struct {
	Seq       int64           `json:"seq"`
	EventType string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	Timestamp string          `json:"ts,omitempty"` // 事件时间，由领域负载携带，这里仅作调试
	PrevHash  string          `json:"prev_hash"`
	Hash      string          `json:"hash"`
}

func (e Envelope) hashInput() []byte {
	return []byte(fmt.Sprintf("%d|%s|%s|%s", e.Seq, e.EventType, e.Data, e.PrevHash))
}

func computeHash(e Envelope) string {
	sum := sha256.Sum256(e.hashInput())
	return hex.EncodeToString(sum[:])
}

// EventStore 并发安全的追加式事件存储。
type EventStore struct {
	mu      sync.Mutex
	path    string
	seq     int64
	lastHash string
	// 订阅者在持锁时回调（回调不得回调 Store，避免重入）。
	subs []func(domainEnvelope)
}

// domainEnvelope 与 envelope 解耦，避免订阅者接触存储细节。
type domainEnvelope struct {
	EventType string
	Data      any
}

// NewEventStore 打开（必要时创建）事件日志；已存在时会校验哈希链并定位末尾。
func NewEventStore(path string) (*EventStore, error) {
	s := &EventStore{path: path}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var prev string
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("event log corrupt at seq after %d: %w", s.seq, err)
		}
		if env.PrevHash != prev {
			return nil, fmt.Errorf("%w: seq %d prev_hash mismatch", ErrTampered, env.Seq)
		}
		if h := computeHash(env); h != env.Hash {
			return nil, fmt.Errorf("%w: seq %d hash mismatch", ErrTampered, env.Seq)
		}
		if env.Seq != s.seq+1 {
			return nil, fmt.Errorf("event log gap: expected seq %d, got %d", s.seq+1, env.Seq)
		}
		s.seq = env.Seq
		prev = env.Hash
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	s.lastHash = prev
	return s, nil
}

// ErrTampered 表示事件日志哈希链断裂（被篡改或写入不完整）。
var ErrTampered = errors.New("event log integrity check failed")

// Append 原子追加一条事件并通知订阅者（持锁顺序执行，先落盘后通知）。
func (s *EventStore) Append(eventType string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	env := Envelope{
		Seq:       s.seq + 1,
		EventType: eventType,
		Data:      raw,
		PrevHash:  s.lastHash,
	}
	env.Hash = computeHash(env)

	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return Envelope{}, err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(env); err != nil {
		f.Close()
		return Envelope{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return Envelope{}, err
	}
	if err := f.Close(); err != nil {
		return Envelope{}, err
	}

	s.seq = env.Seq
	s.lastHash = env.Hash
	// 与重放走同一条解码路径，保证订阅者在实时与回放两种情况下收到类型一致
	// （均为 *domain.X 指针）的负载。
	decoded, err := decodeEvent(env)
	if err != nil {
		return Envelope{}, err
	}
	for _, sub := range s.subs {
		sub(domainEnvelope{EventType: eventType, Data: decoded})
	}
	return env, nil
}

// Subscribe 注册事件订阅者，先回放历史再接收增量。回放期间使用同一把锁，
// 保证不会漏掉订阅注册前后的事件。
func (s *EventStore) Subscribe(handle func(eventType string, data any)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.readAllLocked(func(env Envelope) error {
		v, err := decodeEvent(env)
		if err != nil {
			return err
		}
		handle(env.EventType, v)
		return nil
	}); err != nil {
		return err
	}
	s.subs = append(s.subs, func(de domainEnvelope) { handle(de.EventType, de.Data) })
	return nil
}

// Replay 仅回放事件日志（校验哈希链），用于审计重放报告。
func (s *EventStore) Replay(visit func(seq int64, eventType string, data any) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readAllLocked(func(env Envelope) error {
		v, err := decodeEvent(env)
		if err != nil {
			return err
		}
		return visit(env.Seq, env.EventType, v)
	})
}

// Verify 独立校验整条哈希链。
func (s *EventStore) Verify() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readAllLocked(func(Env Envelope) error { return nil })
}

// Seq 返回当前日志末尾序号。
func (s *EventStore) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *EventStore) readAllLocked(visit func(Envelope) error) error {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	var prev string
	var seq int64
	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if env.PrevHash != prev {
			return fmt.Errorf("%w: seq %d prev_hash mismatch", ErrTampered, env.Seq)
		}
		if h := computeHash(env); h != env.Hash {
			return fmt.Errorf("%w: seq %d hash mismatch", ErrTampered, env.Seq)
		}
		seq++
		if env.Seq != seq {
			return fmt.Errorf("event log gap: expected seq %d, got %d", seq, env.Seq)
		}
		prev = env.Hash
		if err := visit(env); err != nil {
			return err
		}
	}
	return nil
}

func decodeEvent(env Envelope) (any, error) {
	// EventTypes 每次返回全新原型指针，直接反序列化即可。
	prototype, ok := domain.EventTypes()[env.EventType]
	if !ok {
		return nil, fmt.Errorf("unknown event type %q at seq %d", env.EventType, env.Seq)
	}
	if err := json.Unmarshal(env.Data, prototype); err != nil {
		return nil, fmt.Errorf("decode %q at seq %d: %w", env.EventType, env.Seq, err)
	}
	return prototype, nil
}
