package mongoqueue

import (
	"github.com/alouca/goconfig"
	"github.com/alouca/gologger"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"math/rand"
	"time"
)

var (
	l *logger.Logger
	c *config.Config
)

type MongoQueue struct {
	MongoSession *mgo.Session
	C            *mgo.Collection
	Settings     *MongoQueueSettings
}

type MongoQueueSettings struct {
	Cleanup   int // The interval for the cleanup process
	LockLimit int // The maximum number of seconds a job can remain locked to a pid
	// Retry parameters
	RetryLimit   int // The maximum number of retry attempts for a failed task
	MinBackoff   int // The minimum number of seconds to wait before retrying a task after it fails.
	MaxBackoff   int // The maximum number of seconds to wait before retrying a task after it fails.
	MaxDoublings int // The maximum number of times that the interval between failed task retries will be doubled before the increase becomes constant. The constant is: 2**(max_doublings - 1) * min_backoff_seconds.
	AgeLimit     int // The time limit for retrying a failed task, in seconds, measured from the time the task was created.
}

type MongoQueueStats struct {
	Total      int
	InProgress int
	Failed     int
}

func NewMongoQueue(database, queue, server string, settings *MongoQueueSettings) *MongoQueue {
	mq := new(MongoQueue)
	l = logger.GetDefaultLogger()

	if l == nil {
		l = logger.CreateLogger(false, false)
	}

	var err error

	mq.MongoSession, err = mgo.Dial(server)

	if err != nil {
		l.Fatal("Error establishing connection to mongo server\n")
		return nil
	}

	mq.C = mq.MongoSession.DB(database).C(queue)

	mq.Settings = settings

	return mq
}

// Returns the total number of tasks in the queue
func (q *MongoQueue) Count() (c int, err error) {
	c, err = q.C.Count()

	return
}

// Drops all outstanding tasks in the queue
func (q *MongoQueue) Truncate() error {
	err := q.C.DropCollection()

	if err != nil {
		l.Fatal("Error dropping collection: %s\n", err.Error())
	}
	return err
}

func (q *MongoQueue) Add(x interface{}, p int) (string, error) {
	id := bson.NewObjectId()
	now := time.Now().Unix()
	err := q.C.Insert(bson.M{
		"_id":        id,
		"inprogress": false,
		"failed":     false,
		"priority":   p,
		"retries":    0,
		"added":      now,
		"runat":      now,
		"data":       x})

	if err != nil {
		l.Fatal("Error inserting new task: %s\n", err.Error())
	}

	return id.Hex(), err
}

// Pop removes the top-most job from the Priority queue, and returns it back.
func (q *MongoQueue) Pop() (interface{}, error) {
	now := time.Now().Unix()

	change := mgo.Change{
		Update:    bson.M{"$set": bson.M{"inprogress": true, "started": time.Now().Unix()}},
		ReturnNew: true,
	}

	var res bson.M
	_, err := q.C.Find(bson.M{"inprogress": false, "runat": bson.M{"$lte": now}}).Sort("-priority").Limit(1).Apply(change, &res)

	l.Printf("Debug: %v\n", res)

	if err != nil {
		l.Error("Error retrieving data for Pop(): %s\n", err)
		return nil, err
	}
	if res != nil {
		q.C.Remove(res)
		return res["data"], nil
	}

	return nil, nil
}

// Lock gets the top-most job from the Priority Queue, and locks it to a worker. The job is not deleted from the
// queue unless it is marked as completed.
func (q *MongoQueue) Lock(pid string) (string, interface{}, error) {
	now := time.Now().Unix()

	change := mgo.Change{
		Update:    bson.M{"$set": bson.M{"inprogress": true, "started": time.Now().Unix(), "process-id": pid}},
		ReturnNew: true,
	}

	var res bson.M
	info, err := q.C.Find(bson.M{
		"inprogress": false,
		"failed":     false,
		"runat":      bson.M{"$lte": now},
		"retries":    bson.M{"$lte": q.Settings.RetryLimit},
	}).Sort("-priority").Limit(1).Apply(change, &res)

	l.Printf("Debug: %v\n", res)

	if info != nil {
		if info.Updated == 0 {
			return "", nil, nil
		}
	} else if err != nil {
		l.Error("Error retrieving data for Lock(): %s\n", err)
		return "", nil, err
	}
	id := res["_id"].(bson.ObjectId)
	return id.Hex(), res["data"], nil
}

// Complete call removes the job from the priority queue
func (q *MongoQueue) Complete(id string) error {
	oid := bson.ObjectIdHex(id)
	err := q.C.Remove(bson.M{"inprogress": true, "_id": oid})

	if err != nil {
		l.Error("Unable to find job to mark as complete for Job ID %s\n", id)
		return err
	}

	l.Debug("Removed job as completed from id %s\n", id)
	return nil
}

// Marks a job as failed, and keeps in the queue for re-execution at a later stage
func (q *MongoQueue) Fail(id string) error {
	now := time.Now().Unix()

	// Calculate next retry time
	backoff := rand.Int63n(int64(q.Settings.MaxBackoff - q.Settings.MinBackoff))
	runat := now + backoff
	change := mgo.Change{
		Update: bson.M{
			"$inc": bson.M{"retries": 1},
			"$set": bson.M{
				"started":    time.Now().Unix(),
				"inprogress": false,
				"runat":      runat,
			},
		},
		ReturnNew: true,
	}
	var res bson.M
	jid := bson.ObjectIdHex(id)
	info, err := q.C.Find(bson.M{"inprogress": true, "_id": jid}).Limit(1).Apply(change, &res)

	if info != nil && info.Updated == 1 {
		l.Printf("Marked job as failed for ID %s\n", id)
	}
	return err
}

// Cleanup seeks for jobs where the lock has expired, and releases it
func (q *MongoQueue) Cleanup() error {
	now := time.Now().Unix()
	// Set the deadline in relevant time. All tasks before the deadline will be unlocked, and re-scheduled for execution
	deadline := now - int64(q.Settings.LockLimit)

	// Release locks
	info, err := q.C.UpdateAll(bson.M{"inprogress": true, "started": bson.M{"$lte": deadline}}, bson.M{"$set": bson.M{"inprogress": false, "process-id": nil, "started": nil}})

	if err != nil {
		l.Error("Error executing expire locks query: %s\n", err.Error())
	} else {
		l.Debug("Removed total %d lock(s)\n", info.Updated)
	}

	deadline = now - int64(q.Settings.AgeLimit)

	// Failed jobs
	info, err = q.C.UpdateAll(bson.M{"inprogress": false, "failed": false, "added": bson.M{"$lte": deadline}}, bson.M{"$set": bson.M{"failed": true}})
	if err != nil {
		l.Error("Error executing expire jobs query: %s\n", err.Error())
	} else {
		l.Debug("Failed total %d jobs(s)\n", info.Updated)
	}
	return err
}

// Stats
func (q *MongoQueue) Stats() (*MongoQueueStats, error) {
	mqs := new(MongoQueueStats)
	var err error

	mqs.Total, err = q.C.Find(bson.M{}).Count()
	if err != nil {
		l.Error("Error counting records: %s\n", err.Error())
		return nil, err
	}

	mqs.InProgress, err = q.C.Find(bson.M{"inprogress": true}).Count()
	if err != nil {
		l.Error("Error counting records: %s\n", err.Error())
		return nil, err
	}

	return mqs, nil
}