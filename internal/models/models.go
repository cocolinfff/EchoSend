// Package models defines all shared data structures used across the EchoSend daemon.
package models

// ─────────────────────────────────────────────────────────────────────────────
// Network Packet
// ─────────────────────────────────────────────────────────────────────────────

// PacketType enumerates all UDP gossip packet types.
type PacketType string

const (
	PacketPresence PacketType = "PRESENCE"  // 心跳/节点发现
	PacketMsg      PacketType = "MSG"       // 文本消息广播
	PacketFileMeta PacketType = "FILE_META" // 文件元数据广播
	PacketSyncReq  PacketType = "SYNC_REQ"  // 历史同步请求
)

// Packet is the top-level envelope for every UDP gossip message.
// All fields are JSON-serialized before being written to a UDP datagram.
type Packet struct {
	PacketID  string     `json:"packet_id"` // 全局唯一消息 ID（UUID v4），用于去重
	SenderID  string     `json:"sender_id"` // 发送节点的 NodeID
	SenderIP  string     `json:"sender_ip"` // 发送节点的 IP 地址（接收方用于单播回复）
	Type      PacketType `json:"type"`      // 数据包类型
	Payload   []byte     `json:"payload"`   // 业务载荷（JSON 编码的具体结构体）
	Timestamp int64      `json:"timestamp"` // Unix 纳秒时间戳
	TTL       int        `json:"ttl"`       // 剩余跳数，防止泛洪无限扩散（初始值建议 8）
}

// ─────────────────────────────────────────────────────────────────────────────
// File Metadata
// ─────────────────────────────────────────────────────────────────────────────

// FileMeta is the payload carried inside a FILE_META packet.
// 接收方根据 FileSize 与本地阈值决定是否立即自动拉取。
type FileMeta struct {
	FileName string `json:"file_name"` // 原始文件名（仅文件名，不含路径）
	FileSize int64  `json:"file_size"` // 文件字节数
	FileHash string `json:"file_hash"` // SHA-256 十六进制字符串（用于完整性校验）
	TCPPort  int    `json:"tcp_port"`  // 发布节点开放的 TCP 文件服务端口
}

// ─────────────────────────────────────────────────────────────────────────────
// Node (Peer)
// ─────────────────────────────────────────────────────────────────────────────

// NodeInfo describes a discovered peer in the LAN.
// 存储于 BoltDB nodes 桶，以 NodeID 为键。
type NodeInfo struct {
	NodeID   string `json:"node_id"`   // 节点唯一 ID（首次启动自动生成，持久化）
	NodeName string `json:"node_name"` // 节点友好名称（来自配置文件）
	IP       string `json:"ip"`        // 最近一次可达的 IP 地址
	UDPPort  int    `json:"udp_port"`  // 对方 UDP 监听端口
	TCPPort  int    `json:"tcp_port"`  // 对方 TCP 文件服务端口
	LastSeen int64  `json:"last_seen"` // 最近一次收到 PRESENCE 的 Unix 纳秒时间戳
}

// ─────────────────────────────────────────────────────────────────────────────
// Message
// ─────────────────────────────────────────────────────────────────────────────

// Message represents a text broadcast received or sent by this node.
// 存储于 BoltDB messages 桶，以 MessageID 为键。
type Message struct {
	MessageID  string `json:"message_id"`  // 与 Packet.PacketID 相同，保证全局唯一
	SenderID   string `json:"sender_id"`   // 发送节点 NodeID
	SenderName string `json:"sender_name"` // 发送节点名称（展示用）
	SenderIP   string `json:"sender_ip"`   // 发送节点 IP
	Content    string `json:"content"`     // 消息文本内容
	Timestamp  int64  `json:"timestamp"`   // Unix 纳秒时间戳
}

// SyncRequest is the payload carried inside a SYNC_REQ packet.
// RequesterID identifies which node is asking for history replay.
// Limit controls how many recent records peers should replay.
type SyncRequest struct {
	RequesterID string `json:"requester_id"`
	Limit       int    `json:"limit"`
}

// ─────────────────────────────────────────────────────────────────────────────
// File Record
// ─────────────────────────────────────────────────────────────────────────────

// FileStatus indicates the local download state of a file.
type FileStatus string

const (
	FileStatusKnown       FileStatus = "KNOWN"       // 已知元数据但尚未下载
	FileStatusDownloading FileStatus = "DOWNLOADING" // 正在下载中
	FileStatusComplete    FileStatus = "COMPLETE"    // 下载完成且 Hash 校验通过
	FileStatusFailed      FileStatus = "FAILED"      // 下载失败或 Hash 不匹配
	FileStatusSeeding     FileStatus = "SEEDING"     // 本节点为源（本地文件已广播）
)

// FileRecord is persisted in BoltDB files 桶，以 FileHash 为键，
// 记录文件的完整生命周期状态。
type FileRecord struct {
	FileHash  string     `json:"file_hash"`  // SHA-256，同时作为唯一键
	FileName  string     `json:"file_name"`  // 文件名
	FileSize  int64      `json:"file_size"`  // 文件字节数
	SenderID  string     `json:"sender_id"`  // 最初广播该文件的节点 ID
	SenderIP  string     `json:"sender_ip"`  // 来源节点 IP（用于 TCP 拉取）
	SenderTCP int        `json:"sender_tcp"` // 来源节点 TCP 端口
	LocalPath string     `json:"local_path"` // 本地落盘绝对路径（未下载时为空）
	Status    FileStatus `json:"status"`     // 当前下载状态
	Timestamp int64      `json:"timestamp"`  // 首次收到元数据的 Unix 纳秒时间戳
}

// ─────────────────────────────────────────────────────────────────────────────
// IPC Request / Response（CLI ↔ Daemon 本地 HTTP 通信）
// ─────────────────────────────────────────────────────────────────────────────

// IPCSendMessageReq is the JSON body for POST /api/send/message.
type IPCSendMessageReq struct {
	Content string `json:"content"`
}

// IPCSendFileReq is the JSON body for POST /api/send/file.
// 传递的是本机文件的绝对路径，Daemon 负责读取和广播。
type IPCSendFileReq struct {
	Path string `json:"path"`
}

// IPCAddPeerReq is the JSON body for POST /api/peers/add.
// Target 可以是单 IP（如 "192.168.1.100"）或 CIDR（如 "10.0.0.0/24"）。
type IPCAddPeerReq struct {
	Target string `json:"target"`
}

// IPCReply is a generic JSON response envelope.
type IPCReply struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// IPCHistoryResp wraps both messages and file records for GET /api/history.
type IPCHistoryResp struct {
	Messages []Message    `json:"messages"`
	Files    []FileRecord `json:"files"`
}

// IPCStatusResp is returned by GET /api/status.
type IPCStatusResp struct {
	NodeID    string     `json:"node_id"`
	NodeName  string     `json:"node_name"`
	PeerCount int        `json:"peer_count"`
	Peers     []NodeInfo `json:"peers"`
}
