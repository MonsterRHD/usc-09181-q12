package screening

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
)

// ErrConflict 并发冲突：命令基于的事件版本已过期（乐观锁）。
var ErrConflict = errors.New("screening: event store version conflict")

// ErrTampered 事件流被篡改或事件缺失，审计重放时发现哈希链不连续。
var ErrTampered = errors.New("screening: event chain verification failed")

// MemoryEventStore 只追加的内存事件存储。生产环境可替换为同样实现 EventStore 的持久存储；
// 故障恢复只需重新加载全部事件并重放。
type MemoryEventStore struct {
	mu     sync.Mutex
	events []Event
}

func NewMemoryEventStore() *MemoryEventStore { return &MemoryEventStore{} }

// AppendExpect 在期望版本等于当前末尾序号时原子追加，否则返回 ErrConflict。
// expectedSeq < 0 表示不做版本检查。
func (s *MemoryEventStore) AppendExpect(expectedSeq int, events []Event) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := len(s.events)
	if expectedSeq >= 0 && expectedSeq != cur {
		return 0, 0, ErrConflict
	}
	from := cur + 1
	prev := ""
	if cur > 0 {
		prev = s.events[cur-1].Hash
	}
	for i := range events {
		events[i].Seq = cur + i + 1
		events[i].PrevHash = prev
		events[i].Hash = canonicalHash(prev, events[i])
		prev = events[i].Hash
		s.events = append(s.events, events[i])
	}
	return from, cur + len(events), nil
}

func (s *MemoryEventStore) Append(events ...Event) (int, int, error) {
	return s.AppendExpect(-1, events)
}

func (s *MemoryEventStore) Events() ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

func (s *MemoryEventStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// FileEventStore 把事件以 JSON Lines 持久化到文件，启动时回放；崩溃后追加写入可继续。
type FileEventStore struct {
	mu   sync.Mutex
	path string
	mem  *MemoryEventStore
}

func OpenFileEventStore(path string) (*FileEventStore, error) {
	s := &FileEventStore{path: path, mem: NewMemoryEventStore()}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		s.mem.events = append(s.mem.events, e)
	}
	if err := VerifyChain(s.mem.events); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileEventStore) AppendExpect(expectedSeq int, events []Event) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, to, err := s.mem.AppendExpect(expectedSeq, events)
	if err != nil {
		return 0, 0, err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range s.mem.events[from-1 : to] {
		if err := enc.Encode(e); err != nil {
			return 0, 0, err
		}
	}
	return from, to, nil
}

func (s *FileEventStore) Append(events ...Event) (int, int, error) {
	return s.AppendExpect(-1, events)
}

func (s *FileEventStore) Events() ([]Event, error) { return s.mem.Events() }

// canonicalHash 计算事件的 SHA-256 链哈希：前一事件哈希 + 本事件规范化内容。
// 任何对历史事件的修改或删除都会让 VerifyChain 失败。
func canonicalHash(prev string, e Event) string {
	// 链字段本身不参与本事件哈希，避免自引用。
	e.Seq = 0
	e.PrevHash = ""
	e.Hash = ""
	canon, _ := json.Marshal(e)
	sum := sha256.Sum256([]byte(prev + ":" + string(canon)))
	return hex.EncodeToString(sum[:])
}

// VerifyChain 重放并校验事件流：序号连续、PrevHash 衔接、每事件哈希正确。
func VerifyChain(events []Event) error {
	prev := ""
	for i, e := range events {
		if e.Seq != i+1 {
			return ErrTampered
		}
		if e.PrevHash != prev {
			return ErrTampered
		}
		if e.Hash != canonicalHash(prev, e) {
			return ErrTampered
		}
		prev = e.Hash
	}
	return nil
}
