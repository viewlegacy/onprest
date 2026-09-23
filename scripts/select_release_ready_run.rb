#!/usr/bin/env ruby

require 'json'

sha, runs_path, jobs_dir = ARGV
abort('exact commit SHA is required') unless sha&.match?(/\A[0-9a-f]{40}\z/)
abort('workflow runs JSON is required') unless runs_path
abort('workflow jobs directory is required') unless jobs_dir

runs = JSON.parse(File.read(runs_path)).fetch('workflow_runs')
runs.each do |run|
  next unless run['head_sha'] == sha
  next unless run['status'] == 'completed' && run['conclusion'] == 'success'

  jobs_path = File.join(jobs_dir, "#{run.fetch('id')}.json")
  next unless File.file?(jobs_path)

  jobs = JSON.parse(File.read(jobs_path)).fetch('jobs')
  next unless jobs.any? { |job| job['name'] == 'release-ready' && job['conclusion'] == 'success' }

  puts "#{run.fetch('id')}\t#{run.fetch('html_url')}"
  exit 0
end

warn "No successful release-ready job exists for exact SHA #{sha}"
exit 1
