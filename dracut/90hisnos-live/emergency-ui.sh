#!/bin/bash
# 90hisnos-live/emergency-ui.sh

REASON="${1:-Unknown Failure}"

# Ensure output goes directly to console for emergency read
exec < /dev/console
exec > /dev/console
exec 2> /dev/console

echo -e "\n\n\e[41m\e[97m========================================="
echo -e "       HisnOS CRITICAL BOOT ERROR        "
echo -e "=========================================\e[0m"
echo -e "\nError Reason: \e[91m$REASON\e[0m\n"
echo -e "The system failed to boot and has entered the Emergency Recovery menu."
echo -e "Network services are down. Only minimal tools are available.\n"

echo "1) Retry Boot (Re-run Dracut hooks)"
echo "2) Drop to Root Shell (For manual repair)"
echo "3) Reboot System"

while true; do
    read -rp "Select an option [1-3]: " CHOICE
    case $CHOICE in
        1)
            echo "Retrying boot process..."
            exit 0
            ;;
        2)
            echo "Dropping to shell. Type 'exit' to resume boot."
            bash --noprofile --norc
            ;;
        3)
            echo "Rebooting..."
            sleep 2
            reboot -f
            ;;
        *)
            echo "Invalid choice. Please select 1, 2, or 3."
            ;;
    esac
done
