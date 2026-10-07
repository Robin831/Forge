package temper

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLockWaitMeter_CountsFinishedAndInProgressWaits(t *testing.T) {
	m := &LockWaitMeter{}
	assert.Zero(t, m.Waited())

	m.beginWait()
	time.Sleep(30 * time.Millisecond)
	assert.GreaterOrEqual(t, m.Waited(), 30*time.Millisecond, "a wait still in progress counts")
	m.endWait()

	done := m.Waited()
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, done, m.Waited(), "nothing accrues between waits")
}

func TestLockWaitMeter_NilAndContext(t *testing.T) {
	var m *LockWaitMeter
	m.beginWait()
	m.endWait()
	assert.Zero(t, m.Waited(), "a nil meter records nothing and does not panic")

	meter := &LockWaitMeter{}
	assert.Same(t, meter, lockWaitMeterFrom(WithLockWaitMeter(context.Background(), meter)))
	assert.Nil(t, lockWaitMeterFrom(context.Background()))
}
