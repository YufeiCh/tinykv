#### Project1

实现一个存储引擎及上层的 Raw 方法，Raw 方法实现只是将请求封装成对下层存储的调用，处理边界条件。

存储引擎实现的时候可以参考 memStorage 的实现。

#### Project2

##### part A

实现 raft 算法，可以按照测试用例进行完善，不需要强求一开始把所有功能实现完毕。

Leader election

完成 raft 节点的初始化，已经 leader 选举过程中各个节点的转换，message 的处理等



##### part C

接收发送快照的方式与四种基本操作无异，但是快照很大，跟基本操作走一样的流程，容易卡住基本操作的正常进行，所以快照有单独的 worker，并且把快照拆成一个个块进行发送。

###### 在raft module里面的实现：
leader 可以直接通过 Storage 的方法获得当前的快照，follower 通过 handleSnapshot 来处理快照，将 SnapshotMetaData 里的 raft state，commit index，term，当前的成员信息等存储起来。

需要实现的两个部分：
log: 当raft module advance之后, log中存储的日志已经有更新了(存储的部分日志已经成为了上一个快照，已经从storage中去掉了)，这时需要去更新entry中的内容保持一致.
raft: 按照上面的handleSnapshot方法来处理

###### 在raftstore里面的实现：

worker1: raftlog-gc worker
CompactLogRequest 封装在 RaftCmdRequest 中，当收到这个admin command时，不需要去修改状态机，而是去修改RaftTruncateState里面的信息，然后对raftlog-gc worker发送一个 ScheduleCompactLog，由gc worker去执行日志的删除。


worker2: region worker
peerstorage里面实现了Snapshot接口，可以返回快照。
当snapshot生成完之后会在 onRaftMsg 中处理，然后就会调用step将这个msg传给raft module，在handleSnapshot里面进行处理，接下来再影响下一个ready，就会开始处理这个快照。
当确认可以应用快照的时候：就可以更改`RaftLocalState`, `RaftApplyState`, and `RegionLocalState`. 并将这些状态持久到kvdb，删除过时的状态。还需要更新SnapState，并通过 regionSched 发送RegionTaskApply到 region worker, 以及等待任务的完成。

#### project3

##### partA
 multiraft的成员变更和leader迁移.

 leader迁移：需要将`MsgTransferLeader`调用当前leader的step，当前leader收到后需要检查迁移节点是否满足迁移条件：日志是否最新等。如果不是最新，当前leader通过 `MsgAppend` 将需要的日志发给迁移节点并停止接受新日志，以免进入循环(需要一直同步新日志给迁移节点)。如果是最新的，就要立即发送 `MsgTimeOutNow` 给迁移节点，迁移节点收到后开启新一轮选举，由于迁移节点有一个更大的 term 值以及日志，将有非常大的可能成为新一任leader.
参考：CONSENSUS: BRIDGING THEORY AND PRACTICE 的section3.10


成员变更(conf change)：成员节点只能一个一个的添加，通过 `raft.RawNode.ProposeConfChange` 实现，该方法会发起一个 `pb.Entry.EntryType` 被置为 `EntryConfChange` 的提案到 raft group，当这条日志被commit之后们就需要调用`RawNode.ApplyConfChange` 应用这个变更，然后才可以调用 `raft.Raft.addNode` and `raft.Raft.removeNode`.
通过 `raft.go` 里面的 `PendingConfIndex` 来控制每次只能一条日志，当节点移除以后，原来不能commit的此时就满足了半数节点的要求，重新检查一遍没有commit的日志，更新commit日志。

##### partB
leader的迁移：
发起leader迁移，不需要将这个同步到其他的节点上去，只需要调用rawnode中的leader迁移方法，而不是调用propose。

配置变更：
`RegionEpoch` 是 `Region` 中meta信息的一部分，当增删节点或者Region分裂的时候，这个结构体中的数据就会改变，当配置变更时，其中的 `conf_ver` 会递增，当分裂时，`version` 就会递增，这两个值用来在脑裂时保证最新的Region信息。
需要做的事情：
1. 发起配置变更提案：`ProposeConfChange`
2. 当这条日志提交后，修改 `RegionLocalState`，包括其中的 `RegionEpoch` 以及 `Peers` 中的 `Region`
3. 调用 rawNode中的 applyConfChange
提示：`AddNode`中，新增加的节点通过leader的心跳被创建，在 `maybeCreatePeer` 中可以获得提示。新创建的节点是没有被初始化的，所以直接初始化term和index为0，然后leader就可以知道这个节点没有数据，接下来就会发送快照进行同步。
当 `RemoveNode`时，需要直接调用 `destroyPeer` 来停止 raft 模块。
及时更新 GlobalContext 中的 storeMeta
考虑配置变更的幂等性
实现过程中的问题：
`TestConfChangeRecover3B`会出现get log term error，排查以后得到的原因：
新启动的 peer 由于没有日志，所以在 leader 第一次发心跳的时候，会返回 index:0 给leader，然后 leader 处理这个 response 的时候就会发现该节点没有日志从而发送快照，新 peer 收到快照后进行应用并发送 response 给leader。但是 leader 不一定那么快收到这个 response，所以在收到之前再次向所有的follower发送日志的时候，还是会发现这个新peer没有日志，继续发送快照。这时新 peer收到了快照并应用，继续发送response。此时leader收到了第一个快照的 response，并修改关于这个新peer的下一条期望日志，然后向这个新 peer 发送按照第一个快照 response 应用后的日志点发送日志。因此，这个时候新peer就会收到自己的commit时间点前的日志，但是这些commit时间点前的日志已经被删掉了，显然是查不到对应的term，所以得到了上面的get log term error。
解决：当查不到对应的term的时候，并且 storage 返回的错误为 ErrCompacted 的时候，不 append 日志直接发送response给leader反馈需要的最新日志点，这个点为目前所记录的commitIndex。
解决方法安全的推论：commitIndex一定是获得整个集群认可的位置点，所以返回给leader不会影响一致性，不会出现垃圾日志。


Region的分裂：
通常不涉及数据的迁移，只涉及meta data的变更
类似配置变更。
createPeer方法创建新节点，并且注册到router.regions，并且该region的信息需要插入到 ctx.StoreMeta 的 regionRanges 中。
region分裂需要考虑脑裂的场景：应用快照时可能会与已有 region 的范围重叠，需要调用checkSnapshot进行检查。
调用 ExceedEndKey 来比对end key。
处理错误 `ErrRegionNotFound`,`ErrKeyNotInRegion`, `ErrEpochNotMatch`
需要注意的是：propose 和 process 的时候都要检查当前的key还在不在 region 中，即使分裂了底层还是用的同一个store，所以分裂以后如果不检查 key 在不在 region 中的话，依旧可以获得 value，测试用例就无法通过。比如snap的时候，就没有key，因此根据key去检查checkKeyInRegion的时候，需要确认key不为nil，否则会导致snap的cmd没办法正常执行。

##### partC
region 调度器：
调度器需要发送相应的 RegionHeartbeatRequest 给各个存储节点，根据返回的信息决定是否分裂，以及节点的变更。
信息收集：当调度器收到一个心跳后，需要更新它的本地存储信息，然后会检查对于这个region是不是还有未尽的cmd，有的话就返回这个信息作为response。
使用提供的方法跟着提示做就好。
均衡调度：一开始会有一个初始化的 GetMinInterVal，如果返回的是空，就使用 GetNextInterval 去调大MInInterval。如果返回的是一个 operator，那么就会将这个operator 作为下一次获得相关 heartbeat 的 response 进行返回。
这里需要注意判断所选region的副本要对于集群的最大副本，才做迁移。

#### project4

##### partA
MVCC机制：从tinyScheduler获取TSO。使用 KvGet, KvScan来获取数据，根据 KvPrewrite 提交 prewrite请求。
整体机制与pingcap的相关文档解释的差不多，这里需要实现的是 MvccTxn api。所有的相关修改都在 MvccTxn 中，当与某个 command 的修改都到位之后，就可以写入底层的存储

##### partB
实现 KvGet, KvPrewrite, KvCommit。
KvGet：首先查看Lock中有没有小于当前 ts 的锁，如果有说明目前这个 key 被锁住了，那么就需要返回相应的锁信息，方便之后重试。另外，在这种情况下，tinykv也需要为这个 key 找打一个合适的version。
KvPrewrite：将数据写入底层数据库，并且需要锁上这个key，需要检查没有另一个事务已经对这个key上锁，或者正在写同样的key。另外还需要检查是否已经有一个事务提交了这个Key，只有当所有的key都没有被锁上时，才是成功的prewrite。

##### partC
实现KvScan, KvCheckTxnStatus, KvBatchRollBack, KvResolveLock
KvScan：需要实现一个scanner作为迭代的抽象。当scan时，如果遇到错误可以将其记录下来，而不是整个scanning 直接 abort。这里读取的是快照，所以要以write里面的版本作为依据。
以下是写事务时遇到冲突时调用的方法，每一个方法都会修改已存在锁的状态。
KvCheckTxnStatus：检查超时的锁并移除，最后返回这个锁的状态。
KvBatchRollback：检查 key 是否被当前的事务锁着，如果是就把锁去掉，删除相应的数据，并将回滚信息写入write列。
KvResolveLock：检查一批上锁的 key，要么全部回滚要么全部commit。这里可以利用 KvBatchRollback 和 KvCommit的代码。
时间戳包含物理部分与逻辑部分，物理部分是一个单调版本的物理时钟，当比较两个时间戳时，要比对物理与逻辑两个部分，当计算超时时，仅计算物理部分。