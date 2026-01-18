from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
import json
import os
from typing import List, Optional, Tuple

import requests
from dotenv import load_dotenv

import audio

load_dotenv()

WHATSAPP_API_BASE_URL = os.getenv(
    "WHATSAPP_BRIDGE_API_URL", "https://localhost:8080/api"
)
WHATSAPP_API_KEY_HEADER = "X-API-Key"
WHATSAPP_API_KEY = os.getenv(
    "WHATSAPP_BRIDGE_API_KEY", "uF1o3Hk6pL8xVz2aRt7nQc4mYj9sXb5w"
)
WHATSAPP_API_TIMEOUT_SECONDS = 30
WHATSAPP_API_VERIFY_TLS = (
    os.getenv("WHATSAPP_BRIDGE_TLS_VERIFY", "false").lower() == "true"
)


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    sender_name: Optional[str] = None
    media_type: Optional[str] = None
    filename: Optional[str] = None


@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        return self.jid.endswith("@g.us")


@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str


@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]


def _parse_timestamp(value: str) -> datetime:
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    return datetime.fromisoformat(value)


def _post_bridge(path: str, payload: dict) -> dict:
    url = f"{WHATSAPP_API_BASE_URL}{path}"
    response = requests.post(
        url,
        json=payload,
        headers={WHATSAPP_API_KEY_HEADER: WHATSAPP_API_KEY},
        timeout=WHATSAPP_API_TIMEOUT_SECONDS,
        verify=WHATSAPP_API_VERIFY_TLS,
    )
    response.raise_for_status()
    return response.json()


def _message_from_api(d: dict) -> Message:
    return Message(
        timestamp=_parse_timestamp(d["timestamp"]),
        sender=d.get("sender", ""),
        sender_name=d.get("sender_name"),
        chat_name=d.get("chat_name"),
        content=d.get("content", ""),
        is_from_me=bool(d.get("is_from_me", False)),
        chat_jid=d.get("chat_jid", ""),
        id=d.get("id", ""),
        media_type=d.get("media_type"),
        filename=d.get("filename"),
    )


def format_message(message: Message, show_chat_info: bool = True) -> str:
    output = ""

    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    content_prefix = ""
    if message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "

    try:
        if message.is_from_me:
            sender_name = "Me"
        else:
            sender_name = message.sender_name or message.sender

        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        print(f"Error formatting message: {e}")

    return output


def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> str:
    output = ""
    if not messages:
        return "No messages to display."

    for message in messages:
        output += format_message(message, show_chat_info)
    return output


def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
) -> str:
    try:
        payload = {
            "after": after,
            "before": before,
            "sender_phone_number": sender_phone_number,
            "chat_jid": chat_jid,
            "query": query,
            "limit": limit,
            "page": page,
            "include_context": include_context,
            "context_before": context_before,
            "context_after": context_after,
        }

        result = _post_bridge("/list-messages", payload)
        if not result.get("success", False):
            return ""

        messages = [_message_from_api(m) for m in result.get("messages", [])]
        return format_messages_list(messages, show_chat_info=True)

    except requests.RequestException as e:
        print(f"Request error: {str(e)}")
        return ""
    except json.JSONDecodeError:
        print("Error parsing response")
        return ""
    except Exception as e:
        print(f"Unexpected error: {str(e)}")
        return ""


def get_message_context(
    message_id: str, before: int = 5, after: int = 5
) -> MessageContext:
    result = _post_bridge(
        "/get-message-context",
        {"message_id": message_id, "before": before, "after": after},
    )

    if not result.get("success", False) or not result.get("context"):
        raise ValueError(f"Message with ID {message_id} not found")

    context = result["context"]
    return MessageContext(
        message=_message_from_api(context["message"]),
        before=[_message_from_api(m) for m in context.get("before", [])],
        after=[_message_from_api(m) for m in context.get("after", [])],
    )


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> List[Chat]:
    payload = {
        "query": query,
        "limit": limit,
        "page": page,
        "include_last_message": include_last_message,
        "sort_by": sort_by,
    }

    result = _post_bridge("/list-chats", payload)
    if not result.get("success", False):
        return []

    out: List[Chat] = []
    for c in result.get("chats", []):
        last_time = c.get("last_message_time")
        out.append(
            Chat(
                jid=c.get("jid", ""),
                name=c.get("name"),
                last_message_time=_parse_timestamp(last_time) if last_time else None,
                last_message=c.get("last_message"),
                last_sender=c.get("last_sender"),
                last_is_from_me=c.get("last_is_from_me"),
            )
        )
    return out


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    result = _post_bridge(
        "/get-chat",
        {"chat_jid": chat_jid, "include_last_message": include_last_message},
    )
    if not result.get("success", False):
        return None

    chat = result.get("chat")
    if not chat:
        return None

    last_time = chat.get("last_message_time")
    return Chat(
        jid=chat.get("jid", ""),
        name=chat.get("name"),
        last_message_time=_parse_timestamp(last_time) if last_time else None,
        last_message=chat.get("last_message"),
        last_sender=chat.get("last_sender"),
        last_is_from_me=chat.get("last_is_from_me"),
    )


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    result = _post_bridge(
        "/get-direct-chat-by-contact", {"sender_phone_number": sender_phone_number}
    )
    if not result.get("success", False):
        return None

    chat = result.get("chat")
    if not chat:
        return None

    last_time = chat.get("last_message_time")
    return Chat(
        jid=chat.get("jid", ""),
        name=chat.get("name"),
        last_message_time=_parse_timestamp(last_time) if last_time else None,
        last_message=chat.get("last_message"),
        last_sender=chat.get("last_sender"),
        last_is_from_me=chat.get("last_is_from_me"),
    )


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    result = _post_bridge(
        "/get-contact-chats", {"jid": jid, "limit": limit, "page": page}
    )
    if not result.get("success", False):
        return []

    out: List[Chat] = []
    for c in result.get("chats", []):
        last_time = c.get("last_message_time")
        out.append(
            Chat(
                jid=c.get("jid", ""),
                name=c.get("name"),
                last_message_time=_parse_timestamp(last_time) if last_time else None,
                last_message=c.get("last_message"),
                last_sender=c.get("last_sender"),
                last_is_from_me=c.get("last_is_from_me"),
            )
        )
    return out


def get_last_interaction(jid: str) -> Optional[str]:
    result = _post_bridge("/get-last-interaction", {"jid": jid})
    if not result.get("success", False):
        return None

    last = result.get("last")
    if not last:
        return None

    message = _message_from_api(last)
    return format_message(message)


def search_contacts(query: str) -> List[Contact]:
    result = _post_bridge("/search-contacts", {"query": query})
    if not result.get("success", False):
        return []

    out: List[Contact] = []
    for c in result.get("contacts", []):
        out.append(
            Contact(
                phone_number=c.get("phone_number", ""),
                name=c.get("name"),
                jid=c.get("jid", ""),
            )
        )
    return out


def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    try:
        if not recipient:
            return False, "Recipient must be provided"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {"recipient": recipient, "message": message}

        response = requests.post(
            url,
            json=payload,
            headers={WHATSAPP_API_KEY_HEADER: WHATSAPP_API_KEY},
            timeout=WHATSAPP_API_TIMEOUT_SECONDS,
            verify=WHATSAPP_API_VERIFY_TLS,
        )

        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get(
                "message", "Unknown response"
            )

        return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, "Error parsing response"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {"recipient": recipient, "media_path": media_path}

        response = requests.post(
            url,
            json=payload,
            headers={WHATSAPP_API_KEY_HEADER: WHATSAPP_API_KEY},
            timeout=WHATSAPP_API_TIMEOUT_SECONDS,
            verify=WHATSAPP_API_VERIFY_TLS,
        )

        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get(
                "message", "Unknown response"
            )

        return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, "Error parsing response"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return (
                    False,
                    f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}",
                )

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {"recipient": recipient, "media_path": media_path}

        response = requests.post(
            url,
            json=payload,
            headers={WHATSAPP_API_KEY_HEADER: WHATSAPP_API_KEY},
            timeout=WHATSAPP_API_TIMEOUT_SECONDS,
            verify=WHATSAPP_API_VERIFY_TLS,
        )

        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get(
                "message", "Unknown response"
            )

        return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, "Error parsing response"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {"message_id": message_id, "chat_jid": chat_jid}

        response = requests.post(
            url,
            json=payload,
            headers={WHATSAPP_API_KEY_HEADER: WHATSAPP_API_KEY},
            timeout=WHATSAPP_API_TIMEOUT_SECONDS,
            verify=WHATSAPP_API_VERIFY_TLS,
        )

        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                print(f"Media downloaded successfully: {path}")
                return path

            print(f"Download failed: {result.get('message', 'Unknown error')}")
            return None

        print(f"Error: HTTP {response.status_code} - {response.text}")
        return None

    except requests.RequestException as e:
        print(f"Request error: {str(e)}")
        return None
    except json.JSONDecodeError:
        print("Error parsing response")
        return None
    except Exception as e:
        print(f"Unexpected error: {str(e)}")
        return None
