package wire

import "time"

const (
	filetimeTicksPerSecond uint64 = 10_000_000
	filetimeUnixOffset     int64  = 11_644_473_600
)

// DecodeFiletime divides before conversion to avoid nanosecond overflow.
func DecodeFiletime(value Filetime) (time.Time, error) {
	if value == FiletimeSuppress || value == FiletimeResume {
		return time.Time{}, errMalformed
	}
	ticks := uint64(value)
	seconds := ticks / filetimeTicksPerSecond
	nanos := ticks % filetimeTicksPerSecond * 100
	return time.Unix(int64(seconds)-filetimeUnixOffset, int64(nanos)).UTC(), nil
}

// EncodeFiletime rejects dates before 1601 and values outside the tick range.
func EncodeFiletime(value time.Time) (Filetime, error) {
	seconds := value.Unix()
	if seconds < -filetimeUnixOffset || seconds > int64(^uint64(0)/filetimeTicksPerSecond)-filetimeUnixOffset {
		return 0, errMalformed
	}
	seconds += filetimeUnixOffset
	ticks := uint64(seconds) * filetimeTicksPerSecond
	fraction := uint64(value.Nanosecond() / 100)
	if fraction > ^uint64(0)-ticks {
		return 0, errMalformed
	}
	result := Filetime(ticks + fraction)
	if result == FiletimeSuppress || result == FiletimeResume {
		return 0, errMalformed
	}
	return result, nil
}

// DecodeTimeUpdate distinguishes all SET_INFO sentinels from ordinary times.
func DecodeTimeUpdate(value Filetime) (TimeUpdate, error) {
	if value == FiletimeUnchanged || value == FiletimeSuppress || value == FiletimeResume {
		return TimeUpdate{Action: TimeKeep}, nil
	}
	decoded, err := DecodeFiletime(value)
	if err != nil {
		return TimeUpdate{}, err
	}
	return TimeUpdate{Time: decoded, Action: TimeSet}, nil
}

// EncodeTimeUpdate uses zero for TimeKeep and cannot encode a sentinel as TimeSet.
func EncodeTimeUpdate(update TimeUpdate) (Filetime, error) {
	switch update.Action {
	case TimeKeep:
		return FiletimeUnchanged, nil
	case TimeSet:
		v, err := EncodeFiletime(update.Time)
		if err != nil {
			return 0, err
		}
		if v == FiletimeUnchanged {
			return 0, errMalformed
		}
		return v, nil
	default:
		return 0, errMalformed
	}
}
