import argparse
import json
import subprocess
import sys

def tail_file(filepath):
    """
    Tails a file and yields new lines.
    """
    try:
        proc = subprocess.Popen(['tail', '-f', filepath], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        for line in iter(proc.stdout.readline, b''):
            yield line.decode('utf-8')
    except FileNotFoundError:
        print(f"Error: 'tail' command not found. Please install it.", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("\nInterrupted by user.")
        proc.kill()
    except Exception as e:
        print(f"An error occurred: {e}", file=sys.stderr)
        sys.exit(1)


def main():
    """
    Main function to parse arguments and process the file.
    """
    parser = argparse.ArgumentParser(description="Tail a file and show changes to offset and metadata based on the key 'k'.")
    parser.add_argument('filepath', help='The path to the file to tail.')
    args = parser.parse_args()

    last_offsets = {}

    for line in tail_file(args.filepath):
        try:
            data = json.loads(line)

            if isinstance(data, list):
                for item in data:
                    process_item(item, last_offsets)
            else:
                process_item(data, last_offsets)

        except json.JSONDecodeError:
            # Ignore lines that are not valid JSON
            pass
        except KeyboardInterrupt:
            print("\nExiting.")
            break

def process_item(item, last_offsets):
    """
    Processes a single JSON object.
    """
    if 'k' in item and 'v' in item:
        key = item['k']
        value = item['v']
        offset = value.get('offset')
        
        if offset is not None:
            last_offset = last_offsets.get(key)
            if last_offset is None or offset != last_offset:
                print(f"Key: {key}, New Offset: {offset}")
                last_offsets[key] = offset

if __name__ == '__main__':
    main()
