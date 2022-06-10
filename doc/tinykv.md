#### Project1

实现一个存储引擎及上层的Raw方法，Raw方法实现只是将请求封装成对下层存储的调用，处理边界条件。

存储引擎实现的时候可以参考memStorage的实现。

#### Project2

##### part A

实现raft算法，可以按照测试用例进行完善，不需要强求一开始把所有功能实现完毕。

Leader election

完成raft节点的初始化，已经leader选举过程中各个节点的转换，message的处理等



##### part C

接收发送快照的方式与四种基本操作无异，但是快照很大，跟基本操作走一样的流程，容易卡住基本操作的正常进行，所以快照有单独的worker，并且把快照拆成一个个块进行发送。

###### 在raft module里面的实现：
leader可以直接通过 Storage 的方法获得当前的快照，follower通过 handleSnapshot 来处理快照，将SnapshotMetaData里的raft state，commit index，term，当前的成员信息等存储起来。

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
