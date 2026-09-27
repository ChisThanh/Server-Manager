package database

// Types returned to the UI (shared by the engines).

type NameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Overview is the status summary of a target; exactly one of the engine
// sections is set.
type Overview struct {
	Engine   string         `json:"engine"`
	Version  string         `json:"version"`
	Uptime   int64          `json:"uptime"` // seconds
	Postgres *PgOverview    `json:"postgres"`
	MySQL    *MyOverview    `json:"mysql"`
	Redis    *RedisOverview `json:"redis"`
}

type DatabaseInfo struct {
	Name        string  `json:"name"`
	Owner       string  `json:"owner"`
	Encoding    string  `json:"encoding"`
	Collation   string  `json:"collation"`
	Size        int64   `json:"size"` // bytes, -1 = unknown (no access)
	Tables      int64   `json:"tables"`
	Connections int64   `json:"connections"`
	AllowConn   bool    `json:"allowConn"`
	CacheHit    float64 `json:"cacheHit"` // 0..1, -1 = unknown
	System      bool    `json:"system"`
}

type PgReplica struct {
	Pid       int64   `json:"pid"`
	User      string  `json:"user"`
	App       string  `json:"app"`
	Client    string  `json:"client"`
	State     string  `json:"state"`
	SyncState string  `json:"syncState"`
	LagBytes  int64   `json:"lagBytes"`  // -1 unknown
	ReplayLag float64 `json:"replayLag"` // seconds
}

type PgOverview struct {
	VersionNum     int64          `json:"versionNum"`
	StartTime      string         `json:"startTime"`
	MaxConnections int64          `json:"maxConnections"`
	Connections    int64          `json:"connections"`
	ByState        []NameCount    `json:"byState"`
	CacheHitRatio  float64        `json:"cacheHitRatio"` // -1 unknown
	InRecovery     bool           `json:"inRecovery"`
	ReplayDelay    float64        `json:"replayDelay"` // standby: seconds since last replayed transaction, -1 n/a
	Databases      []DatabaseInfo `json:"databases"`
	TotalSize      int64          `json:"totalSize"`
	XactCommit     int64          `json:"xactCommit"`
	XactRollback   int64          `json:"xactRollback"`
	Deadlocks      int64          `json:"deadlocks"`
	LocksWaiting   int64          `json:"locksWaiting"`
	LongestQuery   float64        `json:"longestQuery"` // seconds (active queries)
	IdleInTx       int64          `json:"idleInTx"`
	Replicas       []PgReplica    `json:"replicas"`
	StatStatements bool           `json:"statStatements"` // extension installed in the default database
	Settings       []KV           `json:"settings"`
}

type SlowLogSettings struct {
	Enabled         bool    `json:"enabled"`
	LongQueryTime   float64 `json:"longQueryTime"`
	File            string  `json:"file"`
	Output          string  `json:"output"` // FILE, TABLE, FILE,TABLE, NONE
	NotUsingIndexes bool    `json:"notUsingIndexes"`
}

type MyOverview struct {
	Flavor             string          `json:"flavor"` // mariadb | mysql
	VersionComment     string          `json:"versionComment"`
	MaxConnections     int64           `json:"maxConnections"`
	ThreadsConnected   int64           `json:"threadsConnected"`
	ThreadsRunning     int64           `json:"threadsRunning"`
	MaxUsedConnections int64           `json:"maxUsedConnections"`
	Questions          int64           `json:"questions"`
	SlowQueries        int64           `json:"slowQueries"`
	Connections        int64           `json:"connections"`
	AbortedConnects    int64           `json:"abortedConnects"`
	BufferPoolSize     int64           `json:"bufferPoolSize"`
	BufferPoolHitRatio float64         `json:"bufferPoolHitRatio"` // -1 unknown
	BufferPoolUsed     float64         `json:"bufferPoolUsed"`     // 0..1
	Databases          int64           `json:"databases"`
	SlowLog            SlowLogSettings `json:"slowLog"`
	Replication        []KV            `json:"replication"` // replica status fields (empty when not a replica)
	Settings           []KV            `json:"settings"`
}

type RedisDB struct {
	DB      int   `json:"db"`
	Keys    int64 `json:"keys"`
	Expires int64 `json:"expires"`
	AvgTTL  int64 `json:"avgTtl"` // ms
}

type RedisSlow struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`       // unix seconds
	Duration int64  `json:"duration"` // microseconds
	Command  string `json:"command"`
	Client   string `json:"client"`
	Name     string `json:"name"`
}

type InfoSection struct {
	Name  string `json:"name"`
	Items []KV   `json:"items"`
}

type RedisOverview struct {
	Mode             string        `json:"mode"`
	Role             string        `json:"role"`
	ConnectedClients int64         `json:"connectedClients"`
	BlockedClients   int64         `json:"blockedClients"`
	MaxClients       int64         `json:"maxClients"`
	UsedMemory       int64         `json:"usedMemory"`
	UsedMemoryPeak   int64         `json:"usedMemoryPeak"`
	UsedMemoryRSS    int64         `json:"usedMemoryRss"`
	MaxMemory        int64         `json:"maxMemory"`
	MaxMemoryPolicy  string        `json:"maxMemoryPolicy"`
	FragRatio        float64       `json:"fragRatio"`
	EvictedKeys      int64         `json:"evictedKeys"`
	ExpiredKeys      int64         `json:"expiredKeys"`
	OpsPerSec        float64       `json:"opsPerSec"`
	TotalCommands    int64         `json:"totalCommands"`
	Hits             int64         `json:"hits"`
	Misses           int64         `json:"misses"`
	HitRatio         float64       `json:"hitRatio"` // -1 unknown
	RDBLastSave      int64         `json:"rdbLastSave"`
	RDBLastStatus    string        `json:"rdbLastStatus"`
	RDBChanges       int64         `json:"rdbChanges"`
	RDBSaving        bool          `json:"rdbSaving"`
	AOFEnabled       bool          `json:"aofEnabled"`
	AOFLastStatus    string        `json:"aofLastStatus"`
	AOFRewriting     bool          `json:"aofRewriting"`
	ConnectedSlaves  int64         `json:"connectedSlaves"`
	MasterHost       string        `json:"masterHost"`
	MasterLinkStatus string        `json:"masterLinkStatus"`
	Replicas         []string      `json:"replicas"`
	Keyspace         []RedisDB     `json:"keyspace"`
	Slowlog          []RedisSlow   `json:"slowlog"`
	SlowlogError     string        `json:"slowlogError"`
	ConfigError      string        `json:"configError"` // CONFIG disabled/renamed
	Sections         []InfoSection `json:"sections"`
}

type Session struct {
	ID          int64   `json:"id"` // pid (Postgres) / thread id (MySQL)
	User        string  `json:"user"`
	Database    string  `json:"database"`
	App         string  `json:"app"`
	Client      string  `json:"client"`
	State       string  `json:"state"`
	Command     string  `json:"command"` // MySQL command (Query, Sleep…)
	Wait        string  `json:"wait"`
	BackendType string  `json:"backendType"`
	Duration    float64 `json:"duration"` // seconds in current query/state, -1 none
	XactAge     float64 `json:"xactAge"`  // seconds in transaction, -1 none
	Age         float64 `json:"age"`      // seconds connected
	Query       string  `json:"query"`
	BlockedBy   []int64 `json:"blockedBy"`
}

type LockWait struct {
	Pid       int64   `json:"pid"`
	User      string  `json:"user"`
	Database  string  `json:"database"`
	LockType  string  `json:"lockType"`
	Mode      string  `json:"mode"`
	Relation  string  `json:"relation"`
	BlockedBy []int64 `json:"blockedBy"`
	Waiting   float64 `json:"waiting"`
	Query     string  `json:"query"`
}

type Activity struct {
	Sessions       []Session  `json:"sessions"`
	Locks          []LockWait `json:"locks"`
	MaxConnections int64      `json:"maxConnections"`
}

type SlowQuery struct {
	Query    string  `json:"query"`
	Calls    int64   `json:"calls"`
	TotalMs  float64 `json:"totalMs"`
	MeanMs   float64 `json:"meanMs"`
	MaxMs    float64 `json:"maxMs"`
	Rows     int64   `json:"rows"`
	Database string  `json:"database"`
	User     string  `json:"user"`
	HitRatio float64 `json:"hitRatio"` // -1 unknown
}

// SlowReport lists the most expensive queries and how they are collected.
type SlowReport struct {
	// Source: pg_stat_statements | performance_schema | none
	Source string `json:"source"`
	// Reason when Source is none: notPreloaded | notInstalled | psDisabled | error
	Reason    string          `json:"reason"`
	Detail    string          `json:"detail"`
	Database  string          `json:"database"`
	Preloaded bool            `json:"preloaded"`
	Queries   []SlowQuery     `json:"queries"`
	SlowLog   SlowLogSettings `json:"slowLog"` // MySQL
}

type TableInfo struct {
	Schema     string `json:"schema"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // table, matview, partitioned / MySQL engine
	Rows       int64  `json:"rows"` // estimate
	TotalBytes int64  `json:"totalBytes"`
	DataBytes  int64  `json:"dataBytes"`
	IndexBytes int64  `json:"indexBytes"`
	FreeBytes  int64  `json:"freeBytes"` // MySQL data_free
	DeadRows   int64  `json:"deadRows"`  // Postgres
	LastVacuum string `json:"lastVacuum"`
	LastUpdate string `json:"lastUpdate"`
}
