pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
        // Google's mirror of Maven Central — fallback when central rate-limits.
        maven("https://maven-central.storage-download.googleapis.com/maven2/")
    }
}
dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
        maven("https://maven-central.storage-download.googleapis.com/maven2/")
    }
}
rootProject.name = "varde-anchor"
include(":app")
