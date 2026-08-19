from __future__ import annotations


PROMPTS = (
    (
        "lap_time",
        "Lap four. Thirteen point seven one five seconds.",
        "4周目、13.715",
    ),
    (
        "pilot_name",
        "Mad Max sets a new personal best. Thirteen point seven one five seconds.",
        "マッドマックスが自己ベストを更新。13.715秒です",
    ),
    (
        "pit_service",
        "Car three is in the pit. Fuel and damage recovery are in progress.",
        "3号車がピットイン。燃料とダメージを回復中です",
    ),
    (
        "blue_flag",
        "Blue flag. A faster car is approaching from behind.",
        "ブルーフラッグ。後方から速い車両が接近しています",
    ),
    (
        "boost_ready",
        "Boost is ready. Shift up to activate.",
        "ブースト使用可能。シフトアップで発動します",
    ),
    (
        "race_finish",
        "Race finished. Mad Max takes second place.",
        "レース終了。マッドマックスは2位",
    ),
    (
        "leader_change",
        "Aya takes the lead from Momo at sector two.",
        "アヤがセクター2でモモを抜き、トップに立ちました",
    ),
    (
        "gap_closing",
        "The gap to the leader is down to zero point four eight seconds.",
        "トップとの差は0.48秒まで縮まりました",
    ),
    (
        "sector_best",
        "Car one records the fastest sector three time. Four point two six seconds.",
        "1号車がセクター3の最速タイム、4.26秒を記録しました",
    ),
    (
        "confirmed_contact",
        "Confirmed contact between car two and car four. Both continue racing.",
        "2号車と4号車の接触を確認。両車とも走行を続けます",
    ),
    (
        "heavy_damage",
        "Car four has heavy damage and is under speed restriction.",
        "4号車は大きなダメージを受け、速度制限がかかっています",
    ),
    (
        "pit_exit",
        "Car three exits the pit with full recovery and rejoins in fourth place.",
        "3号車が回復を終えてピットアウト。4位でコースに戻ります",
    ),
    (
        "start_countdown",
        "Five red lights are on. The race will start when the lights go out.",
        "レッドシグナルが5灯点灯。消灯でレーススタートです",
    ),
    (
        "final_lap",
        "Final lap. The top two cars are separated by zero point one seven seconds.",
        "ファイナルラップ。トップ2台の差は0.17秒です",
    ),
    (
        "checkered_flag",
        "Checkered flag. Aya wins after twelve laps.",
        "チェッカーフラッグ。12周を走り、アヤが優勝しました",
    ),
    (
        "mixed_callsign",
        "SDK Racing number seven moves ahead of F P V R C number twenty four.",
        "SDKレーシング7号車が、FPVRC24号車の前に出ました",
    ),
    (
        "decimal_gap",
        "Momo is one lap down, but only zero point zero nine seconds behind on track.",
        "モモは1周遅れですが、コース上の前車との差はわずか0.09秒です",
    ),
    (
        "car_numbers",
        "Cars one, two, three, and four have all completed lap ten.",
        "1号車、2号車、3号車、4号車が、全車10周を完了しました",
    ),
    (
        "race_summary",
        "Aya wins, Momo finishes second, and car three sets the fastest lap at twelve point eight four three seconds.",
        "アヤが優勝、モモが2位。3号車が12.843秒のファステストラップを記録しました",
    ),
    (
        "critical_interrupt",
        "Race control update. Car two has stopped at sector one. Yellow flag is active.",
        "レースコントロールから緊急情報。2号車がセクター1で停止。イエローフラッグです",
    ),
)


PROMPT_LABELS = {
    "lap_time": "LAP / time",
    "pilot_name": "Pilot name / personal best",
    "pit_service": "PIT service",
    "blue_flag": "Blue flag",
    "boost_ready": "Boost ready",
    "race_finish": "Race finish",
    "leader_change": "Leader change",
    "gap_closing": "Closing gap",
    "sector_best": "Sector best",
    "confirmed_contact": "Confirmed contact",
    "heavy_damage": "Damage / speed restriction",
    "pit_exit": "PIT exit",
    "start_countdown": "Start countdown",
    "final_lap": "Final lap",
    "checkered_flag": "Checkered flag",
    "mixed_callsign": "Mixed callsign",
    "decimal_gap": "Decimal gap / lap down",
    "car_numbers": "Multiple car numbers",
    "race_summary": "Race summary",
    "critical_interrupt": "Critical interrupt",
}


def prompts_for(language: str) -> list[tuple[str, str]]:
    index = 1 if language == "en-US" else 2
    return [(prompt[0], prompt[index]) for prompt in PROMPTS]
