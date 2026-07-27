package scsi

// SCSI status byte values (SAM), the subset this project's handlers use.
const (
	StatusGood                = 0x00
	StatusCheckCondition      = 0x02
	StatusBusy                = 0x08
	StatusReservationConflict = 0x18
)
