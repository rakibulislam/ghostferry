package ghostferry

import (
	"database/sql"
	"time"

	"github.com/siddontang/go-mysql/mysql"
	"github.com/sirupsen/logrus"
)

type ReplicatedMasterPositionFetcher interface {
	Current(*sql.DB) (mysql.Position, error)
}

// Selects the master position that we have replicated until from a heartbeat
// table of sort.
type ReplicatedMasterPositionViaCustomQuery struct {
	// The custom query executing should return a single row with two values:
	// the string file and the integer position. For pt-heartbeat, this query
	// would be:
	//
	// "SELECT file, position FROM meta.ptheartbeat WHERE server_id = %d" % serverId
	//
	// where serverId is the master server id, and meta.ptheartbeat is the table
	// where pt-heartbeat writes to.
	//
	// For pt-heartbeat in particular, you should not use the
	// relay_master_log_file and exec_master_log_pos of the DB being replicated
	// as these values are not the master binlog positions.
	Query string
}

func (r ReplicatedMasterPositionViaCustomQuery) Current(replicaDB *sql.DB) (mysql.Position, error) {
	var file string
	var pos uint32
	row := replicaDB.QueryRow(r.Query)
	err := row.Scan(&file, &pos)

	return NewMysqlPosition(file, pos, err)
}

// Only set the MasterDB and ReplicatedMasterPosition options in your code as
// the others will be overwritten by the ferry.
type WaitUntilReplicaIsCaughtUpToMaster struct {
	MasterDB                        *sql.DB
	ReplicatedMasterPositionFetcher ReplicatedMasterPositionFetcher

	ReplicaDB *sql.DB

	logger          *logrus.Entry
	targetMasterPos mysql.Position
}

func (w *WaitUntilReplicaIsCaughtUpToMaster) MarkTargetMasterPosition() error {
	w.logger = logrus.WithField("tag", "wait_replica")

	return WithRetries(100, 600*time.Millisecond, w.logger, "read master binlog position", func() error {
		var err error
		w.targetMasterPos, err = ShowMasterStatusBinlogPosition(w.MasterDB)
		return err
	})
}

func (w *WaitUntilReplicaIsCaughtUpToMaster) IsCaughtUp() (bool, error) {
	var currentReplicatedMasterPos mysql.Position
	err := WithRetries(100, 600*time.Millisecond, w.logger, "read replicated master binlog position", func() error {
		var err error
		currentReplicatedMasterPos, err = w.ReplicatedMasterPositionFetcher.Current(w.ReplicaDB)
		return err
	})

	if err != nil {
		return false, err
	}

	if currentReplicatedMasterPos.Compare(w.targetMasterPos) >= 0 {
		w.logger.Infof("target master position reached by replica: %v >= %v\n", currentReplicatedMasterPos, w.targetMasterPos)
		return true, nil
	}

	w.logger.Debugf("replicated master position is: %v < %v\n", currentReplicatedMasterPos, w.targetMasterPos)
	return false, nil
}

func (w *WaitUntilReplicaIsCaughtUpToMaster) Wait() error {
	err := w.MarkTargetMasterPosition()
	if err != nil {
		w.logger.WithError(err).Error("failed to get master binlog coordinates")
		return err
	}

	w.logger.Infof("target master position is: %v\n", w.targetMasterPos)

	for {
		isCaughtUp, err := w.IsCaughtUp()
		if err != nil {
			w.logger.WithError(err).Error("failed to get replica binlog coordinates")
			return err
		}

		if isCaughtUp {
			break
		}

		time.Sleep(600 * time.Millisecond)
	}

	return nil
}
