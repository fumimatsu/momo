package main

import "fmt"

// A qualifying gap is a difference between valid best laps, never track distance.
// Missing values stay missing; an unmeasured car has no spoken qualifying rank.
type raceAudioQualifyingProgress struct {
	Position int
	BestMS   int
	GapMS    int
	HasGap   bool
}

func raceAudioQualifyingProgressForStanding(session string, standing *raceAudioStanding) raceAudioQualifyingProgress {
	if session != "qualify" || standing == nil || standing.Position < 1 ||
		standing.BestLapMS == nil || *standing.BestLapMS <= 0 {
		return raceAudioQualifyingProgress{}
	}
	progress := raceAudioQualifyingProgress{Position: standing.Position, BestMS: *standing.BestLapMS}
	if standing.Position > 1 && standing.BestLapGapToAheadMS != nil &&
		*standing.BestLapGapToAheadMS >= 0 && *standing.BestLapGapToAheadMS < progress.BestMS {
		progress.GapMS, progress.HasGap = *standing.BestLapGapToAheadMS, true
	}
	return progress
}

func raceAudioQualifyingEvent(runID, carID string, serial uint64, progress raceAudioQualifyingProgress) raceAudioEvent {
	english := fmt.Sprintf("Qualifying P %d.", progress.Position)
	japanese := fmt.Sprintf("予選%d位。", progress.Position)
	if progress.Position == 1 {
		english, japanese = "Qualifying leader.", "予選トップ。"
	} else if progress.HasGap {
		if progress.GapMS == 0 {
			english += fmt.Sprintf(" Best lap tied with P %d.", progress.Position-1)
			japanese += fmt.Sprintf("%d位と同タイム。", progress.Position-1)
		} else {
			english += fmt.Sprintf(" Best lap gap to P %d, %s seconds.", progress.Position-1, raceAudioEnglishLapTime(progress.GapMS))
			japanese += fmt.Sprintf("%d位のベストとの差、%d.%03d秒。", progress.Position-1, progress.GapMS/1000, progress.GapMS%1000)
		}
	}
	return raceAudioEvent{
		EventID: fmt.Sprintf("%s:%s:qualifying:%d:%d:%d:%t:%d", runID, carID,
			progress.Position, progress.BestMS, progress.GapMS, progress.HasGap, serial),
		Kind: "qualifying_update", Priority: 50, EnglishText: english, JapaneseText: japanese,
	}
}
