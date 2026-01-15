/*
  - This file is part of YukkiMusic.
    *

  - YukkiMusic — A Telegram bot that streams music into group voice chats with seamless playback and control.
  - Copyright (C) 2025 TheTeamVivek
    *
  - This program is free software: you can redistribute it and/or modify
  - it under the terms of the GNU General Public License as published by
  - the Free Software Foundation, either version 3 of the License, or
  - (at your option) any later version.
    *
  - This program is distributed in the hope that it will be useful,
  - but WITHOUT ANY WARRANTY; without even the implied warranty of
  - MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
  - GNU General Public License for more details.
    *
  - You should have received a copy of the GNU General Public License
  - along with this program. If not, see <https://www.gnu.org/licenses/>.
*/
package utils

import (
	"github.com/Laky-64/gologging"
	"github.com/amarnathcjd/gogram/telegram"
)

const telegramMaxMessageSize = 4000

func trimTelegramText(text string) string {
	if len(text) <= telegramMaxMessageSize {
		return text
	}
	return text[:telegramMaxMessageSize-50] + "\n\n⚠️ Message trimmed because it was too long."
}

func EOR(
	msg *telegram.NewMessage,
	text string,
	opts ...*telegram.SendOptions,
) (m *telegram.NewMessage, err error) {

	// Protect Telegram from huge messages
	text = trimTelegramText(text)

	m, err = msg.Edit(text, opts...)
	if err != nil {
		// msg.Delete() returns (bool, error)
		_, _ = msg.Delete()

		m, err = msg.Respond(text, opts...)
	}

	if err != nil {
		gologging.Error("[EOR] - " + err.Error())
	}
	return m, err
}

