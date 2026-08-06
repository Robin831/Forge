import { useEffect, useState } from 'react'

// useNow returns a wall-clock timestamp that advances on a fixed interval.
//
// A countdown label needs its own clock: the deadline it counts down to only
// changes when the daemon reports a new last-active time, so without this the
// label would sit frozen between 5s preview polls and jump in whole chunks.
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(timer)
  }, [intervalMs])
  return now
}
