package library

import (
	"regexp"
	"strings"
	"time"
)

// Structured event code taxonomy.
const (
	EventCodeRoboticsMoveSuccess   = "ROBOTICS.MOVE.SUCCESS"
	EventCodeRoboticsMoveFailure   = "ROBOTICS.MOVE.FAILURE"
	EventCodeRoboticsLoadSuccess   = "ROBOTICS.LOAD.SUCCESS"
	EventCodeRoboticsLoadFailure   = "ROBOTICS.LOAD.FAILURE"
	EventCodeRoboticsUnloadSuccess = "ROBOTICS.UNLOAD.SUCCESS"
	EventCodeRoboticsUnloadFailure = "ROBOTICS.UNLOAD.FAILURE"
	// EventCodeRoboticsUnloadFallback: the mechanical unload committed, but
	// the requested destination was filled by a concurrent operation
	// during the unload's own latency sleep, so the volume was placed
	// outside the library instead - mirrors EventCodeCleaningTapeEjectFallback
	// for the same class of race on the auto-eject path.
	EventCodeRoboticsUnloadFallback          = "ROBOTICS.UNLOAD.FALLBACK.WARNING"
	EventCodeRoboticsDoorIOOpenSuccess       = "ROBOTICS.DOOR.IO.OPEN.SUCCESS"
	EventCodeRoboticsDoorIOOpenFailure       = "ROBOTICS.DOOR.IO.OPEN.FAILURE"
	EventCodeRoboticsDoorIOCloseSuccess      = "ROBOTICS.DOOR.IO.CLOSE.SUCCESS"
	EventCodeRoboticsDoorIOCloseFailure      = "ROBOTICS.DOOR.IO.CLOSE.FAILURE"
	EventCodeRoboticsDoorStorageOpenSuccess  = "ROBOTICS.DOOR.STORAGE.OPEN.SUCCESS"
	EventCodeRoboticsDoorStorageOpenFailure  = "ROBOTICS.DOOR.STORAGE.OPEN.FAILURE"
	EventCodeRoboticsDoorStorageCloseSuccess = "ROBOTICS.DOOR.STORAGE.CLOSE.SUCCESS"
	EventCodeRoboticsDoorStorageCloseFailure = "ROBOTICS.DOOR.STORAGE.CLOSE.FAILURE"
	EventCodeRoboticsScanStorageSuccess      = "ROBOTICS.SCAN.STORAGE.SUCCESS"
	EventCodeRoboticsFaultSetSuccess         = "ROBOTICS.FAULT.SET.SUCCESS"
	EventCodeRoboticsFaultSetFailure         = "ROBOTICS.FAULT.SET.FAILURE"

	// EventCodeRoboticsMoveStarted is the plain "a move is about to
	// happen" bracket, paired with EventCodeRoboticsMoveSuccess/Failure -
	// the atomic, physical-step-by-step narration ("moving to slot 3",
	// "grabbed tape ABC123", ...) is deliberately NOT part of this
	// audited/SNMP'd taxonomy at all; it's live-only (see
	// Library.recordArmStep/ArmStep), the same way a door phase
	// transition never gets a code here either.
	EventCodeRoboticsMoveStarted = "ROBOTICS.MOVE.STARTED"

	EventCodeMediaOutsideCreateSuccess = "MEDIA.OUTSIDE.CREATE.SUCCESS"
	EventCodeMediaOutsideCreateFailure = "MEDIA.OUTSIDE.CREATE.FAILURE"
	EventCodeMediaOutsideDeleteSuccess = "MEDIA.OUTSIDE.DELETE.SUCCESS"
	EventCodeMediaOutsideDeleteFailure = "MEDIA.OUTSIDE.DELETE.FAILURE"
	EventCodeMediaImportSuccess        = "MEDIA.IMPORT.SUCCESS"
	EventCodeMediaImportFailure        = "MEDIA.IMPORT.FAILURE"
	EventCodeMediaEjectSuccess         = "MEDIA.EJECT.SUCCESS"
	EventCodeMediaEjectFailure         = "MEDIA.EJECT.FAILURE"
	EventCodeMediaVolumeCreateSuccess  = "MEDIA.VOLUME.CREATE.SUCCESS"
	EventCodeMediaVolumeCreateFailure  = "MEDIA.VOLUME.CREATE.FAILURE"
	EventCodeMediaVolumeDeleteSuccess  = "MEDIA.VOLUME.DELETE.SUCCESS"
	EventCodeMediaVolumeDeleteFailure  = "MEDIA.VOLUME.DELETE.FAILURE"
	EventCodeMediaVolumeFullWarning    = "MEDIA.VOLUME.FULL.WARNING"

	EventCodeMediaVolumeWriteProtectSetSuccess = "MEDIA.VOLUME.WRITE_PROTECT.SET.SUCCESS"
	EventCodeMediaVolumeWriteProtectSetFailure = "MEDIA.VOLUME.WRITE_PROTECT.SET.FAILURE"

	EventCodeDriveFaultSetSuccess = "DRIVE.FAULT.SET.SUCCESS"
	EventCodeDriveFaultSetFailure = "DRIVE.FAULT.SET.FAILURE"

	EventCodeDriveFormatMediumSetSuccess = "DRIVE.FORMAT_MEDIUM.SET.SUCCESS"
	EventCodeDriveFormatMediumSetFailure = "DRIVE.FORMAT_MEDIUM.SET.FAILURE"

	EventCodeDriveMAMAttributesSetSuccess = "DRIVE.MAM_ATTRIBUTES.SET.SUCCESS"
	EventCodeDriveMAMAttributesSetFailure = "DRIVE.MAM_ATTRIBUTES.SET.FAILURE"

	EventCodeDriveEncryptedSetSuccess = "DRIVE.ENCRYPTED.SET.SUCCESS"
	EventCodeDriveEncryptedSetFailure = "DRIVE.ENCRYPTED.SET.FAILURE"

	// Drive read/write/idle activity, detected by a per-drive filesystem
	// watcher on the loaded volume's real backing file (see
	// startDriveActivityWatcher) - edge-triggered (one event per
	// idle<->active transition, not per syscall).
	EventCodeDriveActivityReadStarted  = "DRIVE.ACTIVITY.READ.STARTED"
	EventCodeDriveActivityWriteStarted = "DRIVE.ACTIVITY.WRITE.STARTED"
	EventCodeDriveActivityIdle         = "DRIVE.ACTIVITY.IDLE"

	EventCodeCleaningCycleSuccess      = "CLEANING.CYCLE.SUCCESS"
	EventCodeCleaningTapeExpired       = "CLEANING.TAPE.EXPIRED.WARNING"
	EventCodeCleaningTapeUnavailable   = "CLEANING.TAPE.UNAVAILABLE.WARNING"
	EventCodeCleaningTapeCreateSuccess = "CLEANING.TAPE.CREATE.SUCCESS"
	EventCodeCleaningTapeCreateFailure = "CLEANING.TAPE.CREATE.FAILURE"
	EventCodeCleaningTapeEjectFallback = "CLEANING.TAPE.EJECT_FALLBACK.WARNING"
	EventCodeCleaningCycleStart        = "CLEANING.CYCLE.START"

	EventCodeRoboticsLoadStarted   = "ROBOTICS.LOAD.STARTED"
	EventCodeRoboticsUnloadStarted = "ROBOTICS.UNLOAD.STARTED"

	EventCodeAuthBootstrapSuccess      = "AUTH.BOOTSTRAP.SUCCESS"
	EventCodeAuthBootstrapFailure      = "AUTH.BOOTSTRAP.FAILURE"
	EventCodeAuthLoginSuccess          = "AUTH.LOGIN.SUCCESS"
	EventCodeAuthLoginFailure          = "AUTH.LOGIN.FAILURE"
	EventCodeAuthLogoutSuccess         = "AUTH.LOGOUT.SUCCESS"
	EventCodeAuthChangePasswordSuccess = "AUTH.CHANGE_PASSWORD.SUCCESS"
	EventCodeAuthChangePasswordFailure = "AUTH.CHANGE_PASSWORD.FAILURE"

	EventCodeConfigUserCreateSuccess        = "CONFIG.USER.CREATE.SUCCESS"
	EventCodeConfigUserCreateFailure        = "CONFIG.USER.CREATE.FAILURE"
	EventCodeConfigUserDeleteSuccess        = "CONFIG.USER.DELETE.SUCCESS"
	EventCodeConfigUserDeleteFailure        = "CONFIG.USER.DELETE.FAILURE"
	EventCodeConfigUserRoleSetSuccess       = "CONFIG.USER.ROLE_SET.SUCCESS"
	EventCodeConfigUserRoleSetFailure       = "CONFIG.USER.ROLE_SET.FAILURE"
	EventCodeConfigUserPasswordResetSuccess = "CONFIG.USER.PASSWORD_RESET.SUCCESS"
	EventCodeConfigUserPasswordResetFailure = "CONFIG.USER.PASSWORD_RESET.FAILURE"

	EventCodeConfigTokenCreateSuccess = "CONFIG.TOKEN.CREATE.SUCCESS"
	EventCodeConfigTokenCreateFailure = "CONFIG.TOKEN.CREATE.FAILURE"
	EventCodeConfigTokenRevokeSuccess = "CONFIG.TOKEN.REVOKE.SUCCESS"
	EventCodeConfigTokenRevokeFailure = "CONFIG.TOKEN.REVOKE.FAILURE"

	EventCodeConfigSettingsUpdateSuccess = "CONFIG.SETTINGS.UPDATE.SUCCESS"
	EventCodeConfigSettingsUpdateFailure = "CONFIG.SETTINGS.UPDATE.FAILURE"

	EventCodeConfigBackupCreateSuccess         = "CONFIG.BACKUP.CREATE.SUCCESS"
	EventCodeConfigBackupCreateFailure         = "CONFIG.BACKUP.CREATE.FAILURE"
	EventCodeConfigBackupDeleteSuccess         = "CONFIG.BACKUP.DELETE.SUCCESS"
	EventCodeConfigBackupDeleteFailure         = "CONFIG.BACKUP.DELETE.FAILURE"
	EventCodeConfigBackupScheduleUpdateSuccess = "CONFIG.BACKUP_SCHEDULE.UPDATE.SUCCESS"
	EventCodeConfigBackupScheduleUpdateFailure = "CONFIG.BACKUP_SCHEDULE.UPDATE.FAILURE"
	EventCodeConfigBackupScheduledRunSuccess   = "CONFIG.BACKUP.SCHEDULED_RUN.SUCCESS"
	EventCodeConfigBackupScheduledRunFailure   = "CONFIG.BACKUP.SCHEDULED_RUN.FAILURE"
	EventCodeConfigRestoreSuccess              = "CONFIG.RESTORE.SUCCESS"
	EventCodeConfigRestoreFailure              = "CONFIG.RESTORE.FAILURE"
	EventCodeConfigResetSuccess                = "CONFIG.RESET.SUCCESS"
	EventCodeConfigResetFailure                = "CONFIG.RESET.FAILURE"

	EventCodeSystemPersistFailure = "SYSTEM.PERSIST.FAILURE"
)

const (
	EventSeverityInformation   = "information"
	EventSeverityWarning       = "warning"
	EventSeverityError         = "error"
	EventSeverityConfiguration = "configuration"
)

const (
	EventOutcomeSuccess = "success"
	EventOutcomeFailure = "failure"
)

var nonCodeRE = regexp.MustCompile(`[^A-Z0-9]+`)

// CanonicalizeEvent fills structured fields from event code while preserving
// backward compatibility for existing consumers that still read Event.Type.
func CanonicalizeEvent(e Event) Event {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Code == "" {
		if e.Type != "" {
			e.Code = legacyTypeToCode(e.Type, e.Message)
		} else {
			e.Code = "SYSTEM.EVENT.SUCCESS"
		}
	}
	if e.Type == "" {
		e.Type = e.Code
	} else {
		// Keep Type as alias to structured code for older UI/CLI output paths.
		e.Type = e.Code
	}

	parts := strings.Split(e.Code, ".")
	if e.Category == "" && len(parts) > 0 {
		switch parts[0] {
		case "ROBOTICS":
			e.Category = "robotic"
		case "DRIVE":
			e.Category = "drive"
		case "CLEANING":
			e.Category = "cleaning"
		case "MEDIA":
			e.Category = "media"
		case "AUTH":
			e.Category = "auth"
		case "CONFIG":
			e.Category = "configuration"
		case "SECURITY":
			e.Category = "security"
		case "SYSTEM":
			e.Category = "system"
		default:
			e.Category = strings.ToLower(parts[0])
		}
	}

	if e.Outcome == "" {
		switch {
		case strings.HasSuffix(e.Code, ".FAILURE"):
			e.Outcome = EventOutcomeFailure
		case strings.HasSuffix(e.Code, ".SUCCESS"):
			e.Outcome = EventOutcomeSuccess
		default:
			e.Outcome = EventOutcomeSuccess
		}
	}

	if e.Severity == "" {
		switch {
		case strings.HasSuffix(e.Code, ".WARNING"):
			e.Severity = EventSeverityWarning
		case e.Outcome == EventOutcomeFailure:
			e.Severity = EventSeverityError
		case strings.HasPrefix(e.Code, "CONFIG."):
			e.Severity = EventSeverityConfiguration
		default:
			e.Severity = EventSeverityInformation
		}
	}

	if e.Operation == "" {
		e.Operation = operationFromCode(e.Code)
	}

	return e
}

func operationFromCode(code string) string {
	parts := strings.Split(code, ".")
	if len(parts) < 2 {
		return strings.ToLower(code)
	}
	end := len(parts)
	last := parts[len(parts)-1]
	if last == "SUCCESS" || last == "FAILURE" || last == "WARNING" {
		end = len(parts) - 1
	}
	if end <= 1 {
		return strings.ToLower(parts[0])
	}
	return strings.ToLower(strings.Join(parts[1:end], "."))
}

func legacyTypeToCode(typ, message string) string {
	switch typ {
	case "load":
		return EventCodeRoboticsLoadSuccess
	case "unload":
		return EventCodeRoboticsUnloadSuccess
	case "move":
		return EventCodeRoboticsMoveSuccess
	case "import":
		return EventCodeMediaImportSuccess
	case "eject":
		return EventCodeMediaEjectSuccess
	case "create-volume":
		return EventCodeMediaVolumeCreateSuccess
	case "delete-volume":
		return EventCodeMediaVolumeDeleteSuccess
	case "outside-create":
		return EventCodeMediaOutsideCreateSuccess
	case "outside-delete":
		return EventCodeMediaOutsideDeleteSuccess
	case "drive-fault":
		return EventCodeDriveFaultSetSuccess
	case "format-medium":
		return EventCodeDriveFormatMediumSetSuccess
	case "mam-attributes":
		return EventCodeDriveMAMAttributesSetSuccess
	case "encrypted":
		return EventCodeDriveEncryptedSetSuccess
	case "write-protect":
		return EventCodeMediaVolumeWriteProtectSetSuccess
	case "cleaning-cycle":
		return EventCodeCleaningCycleSuccess
	case "cleaning-expired":
		return EventCodeCleaningTapeExpired
	case "cleaning-unavailable":
		return EventCodeCleaningTapeUnavailable
	case "cleaning-tape-create":
		return EventCodeCleaningTapeCreateSuccess
	case "cleaning-eject-fallback":
		return EventCodeCleaningTapeEjectFallback
	case "unload-fallback":
		return EventCodeRoboticsUnloadFallback
	case "cleaning-start":
		return EventCodeCleaningCycleStart
	case "loading":
		return EventCodeRoboticsLoadStarted
	case "unloading":
		return EventCodeRoboticsUnloadStarted
	case "robotic-fault":
		return EventCodeRoboticsFaultSetSuccess
	case "volume-full":
		return EventCodeMediaVolumeFullWarning
	case "persist-error":
		return EventCodeSystemPersistFailure
	case "io-door":
		if strings.Contains(strings.ToLower(message), "opened") {
			return EventCodeRoboticsDoorIOOpenSuccess
		}
		return EventCodeRoboticsDoorIOCloseSuccess
	case "storage-door":
		if strings.Contains(strings.ToLower(message), "opened") {
			return EventCodeRoboticsDoorStorageOpenSuccess
		}
		return EventCodeRoboticsDoorStorageCloseSuccess
	case "storage-scan":
		return EventCodeRoboticsScanStorageSuccess
	case "move-started":
		return EventCodeRoboticsMoveStarted
	case "drive-activity-read":
		return EventCodeDriveActivityReadStarted
	case "drive-activity-write":
		return EventCodeDriveActivityWriteStarted
	case "drive-activity-idle":
		return EventCodeDriveActivityIdle
	case "io-load":
		return "ROBOTICS.DOOR.IO.LOAD.SUCCESS"
	case "io-pickup":
		return "ROBOTICS.DOOR.IO.PICKUP.SUCCESS"
	case "storage-load":
		return "ROBOTICS.DOOR.STORAGE.LOAD.SUCCESS"
	case "storage-pickup":
		return "ROBOTICS.DOOR.STORAGE.PICKUP.SUCCESS"
	default:
		normalized := strings.ToUpper(nonCodeRE.ReplaceAllString(typ, "_"))
		normalized = strings.Trim(normalized, "_")
		if normalized == "" {
			normalized = "UNKNOWN"
		}
		return "SYSTEM.LEGACY." + normalized + ".SUCCESS"
	}
}
